package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cucumber/godog"
	messages "github.com/cucumber/messages-go/v10"
	log "github.com/sirupsen/logrus"

	"github.com/elastic/e2e-testing/internal/shell"
	"github.com/elastic/e2e-testing/internal/utils"
)

const defaultBeatVersion = "8.0.0-SNAPSHOT"
const defaultEventsWaitTimeout = 120 * time.Second
const defaultDeployWaitTimeout = 120 * time.Second

type podsManager struct {
	kubectl kubernetesControl
	ctx     context.Context
}

func (m *podsManager) executeTemplateFor(podName string, writer io.Writer, options []string) error {
	path := filepath.Join("testdata/templates", sanitizeName(podName)+".yml.tmpl")

	usedOptions := make(map[string]bool)
	funcs := template.FuncMap{
		"option": func(o string) bool {
			usedOptions[o] = true
			for _, option := range options {
				if o == option {
					return true
				}
			}
			return false
		},
		"beats_namespace": func() string {
			return utils.GetDockerNamespaceEnvVar("beats")
		},
		"beats_version": func() string {
			return shell.GetEnv("GITHUB_CHECK_SHA1", shell.GetEnv("BEAT_VERSION", defaultBeatVersion))
		},
		"namespace": func() string {
			return m.kubectl.Namespace
		},
		// Can be used to add owner references so cluster-level resources
		// are removed when removing the namespace.
		"namespace_uid": func() string {
			return m.kubectl.NamespaceUID
		},
	}

	t, err := template.New(filepath.Base(path)).Funcs(funcs).ParseFiles(path)
	if os.IsNotExist(err) {
		log.Debugf("template %s does not exist", path)
		return godog.ErrPending
	}
	if err != nil {
		return fmt.Errorf("parsing template %s: %w", path, err)
	}

	err = t.ExecuteTemplate(writer, filepath.Base(path), nil)
	if err != nil {
		return fmt.Errorf("executing template %s: %w", path, err)
	}

	for _, option := range options {
		if _, used := usedOptions[option]; !used {
			log.Debugf("option '%s' is not used in template for '%s'", option, podName)
			return godog.ErrPending
		}
	}

	return nil
}

func (m *podsManager) isDeleted(podName string, options []string) error {
	var buf bytes.Buffer
	err := m.executeTemplateFor(podName, &buf, options)
	if err != nil {
		return err
	}

	_, err = m.kubectl.RunWithStdin(m.ctx, &buf, "delete", "-f", "-")
	if err != nil {
		return fmt.Errorf("failed to delete '%s': %w", podName, err)
	}
	return nil
}

func (m *podsManager) isDeployed(podName string, options []string) error {
	var buf bytes.Buffer
	err := m.executeTemplateFor(podName, &buf, options)
	if err != nil {
		return err
	}

	_, err = m.kubectl.RunWithStdin(m.ctx, &buf, "apply", "-f", "-")
	if err != nil {
		return fmt.Errorf("failed to deploy '%s': %w", podName, err)
	}
	return nil
}

func (m *podsManager) isRunning(podName string, options []string) error {
	err := m.isDeployed(podName, options)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(m.ctx, defaultDeployWaitTimeout)
	defer cancel()

	_, err = m.getPodInstances(ctx, podName)
	if err != nil {
		return fmt.Errorf("waiting for instance of '%s': %w", podName, err)
	}
	return nil
}

func (m *podsManager) resourceIs(podName string, state string, options ...string) error {
	switch state {
	case "running":
		return m.isRunning(podName, options)
	case "deployed":
		return m.isDeployed(podName, options)
	case "deleted":
		return m.isDeleted(podName, options)
	default:
		return godog.ErrPending
	}
}

// This only works as JSON, not as YAML.
// From https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/#ephemeral-containers-api
const ephemeralContainerTemplate = `
{
    "apiVersion": "v1",
    "kind": "EphemeralContainers",
    "metadata": {
        "name": "{{ .podName }}"
    },
    "ephemeralContainers": [{
        "name": "ephemeral-container",
        "command": [
          "/bin/sh", "-c",
          "while true; do echo Hi from an ephemeral container; sleep 1; done"
        ],
        "image": "busybox",
        "imagePullPolicy": "IfNotPresent",
        "stdin": true,
        "tty": true,
        "terminationMessagePolicy": "File"
    }]
}
`

func (m *podsManager) startEphemeralContainerIn(podName string) error {
	podName = sanitizeName(podName)
	t := template.Must(template.New("ephemeral-container").Parse(ephemeralContainerTemplate))
	var buf bytes.Buffer
	err := t.Execute(&buf, map[string]string{"podName": podName})
	if err != nil {
		return fmt.Errorf("executing ephemeral-container template: %w", err)
	}

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/ephemeralcontainers", m.kubectl.Namespace, podName)
	_, err = m.kubectl.RunWithStdin(m.ctx, &buf, "replace", "--raw", path, "-f", "-")
	if err != nil {
		return fmt.Errorf("failed to create ephemeral container: %w. Is EphemeralContainers feature flag enabled in the cluster?", err)
	}
	return nil
}

func (m *podsManager) collectsEventsWith(podName string, condition string) error {
	_, _, ok := splitCondition(condition)
	if !ok {
		return fmt.Errorf("invalid condition '%s'", condition)
	}

	return m.waitForEventsCondition(podName, func(ctx context.Context, localPath string) (bool, error) {
		ok, err := containsEventsWith(localPath, condition)
		if ok {
			return true, nil
		}
		if err != nil {
			log.Debugf("Error checking if %v contains %v: %v", localPath, condition, err)
		}
		return false, nil
	})
}

func (m *podsManager) doesNotCollectEvents(podName, condition, duration string) error {
	_, _, ok := splitCondition(condition)
	if !ok {
		return fmt.Errorf("invalid condition '%s'", condition)
	}

	d, err := time.ParseDuration(duration)
	if err != nil {
		return fmt.Errorf("invalid duration %s: %w", d, err)
	}

	return m.waitForEventsCondition(podName, func(ctx context.Context, localPath string) (bool, error) {
		events, err := readEventsWith(localPath, condition)
		if err != nil {
			return false, err
		}
		// No events ever received, so condition satisfied.
		if len(events) == 0 {
			return true, nil
		}

		lastEvent := events[len(events)-1]
		lastTimestamp, ok := lastEvent["@timestamp"].(string)
		if !ok {
			return false, fmt.Errorf("event %v doesn't contain a @timestamp", lastEvent)
		}
		t, err := time.Parse(time.RFC3339, lastTimestamp)
		if err != nil {
			return false, fmt.Errorf("failed to parse @timestamp %s: %w", lastTimestamp, err)
		}
		if sinceLast := time.Now().Sub(t); sinceLast <= d {
			// Condition cannot be satisfied until the duration has passed after the last
			// event. So wait till then.
			select {
			case <-ctx.Done():
			case <-time.After(d - sinceLast):
			}
			return false, nil
		}

		return true, nil
	})
}

func (m *podsManager) waitForEventsCondition(podName string, conditionFn func(ctx context.Context, localPath string) (bool, error)) error {
	ctx, cancel := context.WithTimeout(m.ctx, defaultEventsWaitTimeout)
	defer cancel()

	instances, err := m.getPodInstances(ctx, podName)
	if err != nil {
		return fmt.Errorf("failed to get pod name: %w", err)
	}

	tmpDir, err := ioutil.TempDir(os.TempDir(), "test-")
	if err != nil {
		return fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	containerPath := fmt.Sprintf("%s/%s:/tmp/beats-events", m.kubectl.Namespace, instances[0])
	localPath := filepath.Join(tmpDir, "events")
	exp := backoff.WithContext(backoff.NewConstantBackOff(1*time.Second), ctx)
	return backoff.Retry(func() error {
		_, err := m.kubectl.Run(ctx, "cp", "--no-preserve", containerPath, localPath)
		if err != nil {
			log.Debugf("Failed to copy events from %s to %s: %s", containerPath, localPath, err)
			return err
		}
		ok, err := conditionFn(ctx, localPath)
		if !ok {
			return fmt.Errorf("events do not satisfy condition")
		}
		return nil
	}, exp)
}

func (m *podsManager) getPodInstances(ctx context.Context, podName string) (instances []string, err error) {
	app := sanitizeName(podName)
	ticker := backoff.WithContext(backoff.NewConstantBackOff(1*time.Second), ctx)
	err = backoff.Retry(func() error {
		output, err := m.kubectl.Run(ctx, "get", "pods",
			"-l", "k8s-app="+app,
			"--template", `{{range .items}}{{ if eq .status.phase "Running" }}{{.metadata.name}}{{"\n"}}{{ end }}{{end}}`)
		if err != nil {
			return err
		}
		if output == "" {
			return fmt.Errorf("no running pods with label k8s-app=%s found", app)
		}
		instances = strings.Split(strings.TrimSpace(output), "\n")
		return nil
	}, ticker)
	return
}

func splitCondition(c string) (key string, value string, ok bool) {
	fields := strings.SplitN(c, ":", 2)
	if len(fields) != 2 || len(fields[0]) == 0 {
		return
	}

	return fields[0], fields[1], true
}

func flattenMap(m map[string]interface{}) map[string]interface{} {
	flattened := make(map[string]interface{})
	for k, v := range m {
		switch child := v.(type) {
		case map[string]interface{}:
			childMap := flattenMap(child)
			for ck, cv := range childMap {
				flattened[k+"."+ck] = cv
			}
		default:
			flattened[k] = v
		}
	}
	return flattened
}

func containsEventsWith(path string, condition string) (bool, error) {
	events, err := readEventsWith(path, condition)
	if err != nil {
		return false, err
	}
	return len(events) > 0, nil
}

func readEventsWith(path string, condition string) ([]map[string]interface{}, error) {
	key, value, ok := splitCondition(condition)
	if !ok {
		return nil, fmt.Errorf("invalid condition '%s'", condition)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var events []map[string]interface{}
	decoder := json.NewDecoder(f)
	for decoder.More() {
		var event map[string]interface{}
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decoding event: %w", err)
		}

		event = flattenMap(event)
		if v, ok := event[key]; ok && fmt.Sprint(v) == value {
			events = append(events, event)
		}
	}

	return events, nil
}

func sanitizeName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "-")
}

func waitDuration(ctx context.Context, duration string) error {
	d, err := time.ParseDuration(duration)
	if err != nil {
		return fmt.Errorf("invalid duration %s: %w", d, err)
	}

	select {
	case <-time.After(d):
	case <-ctx.Done():
	}

	return nil
}

var cluster kubernetesCluster

func InitializeTestSuite(ctx *godog.TestSuiteContext) {
	suiteContext, cancel := context.WithCancel(context.Background())
	log.DeferExitHandler(cancel)

	ctx.BeforeSuite(func() {
		err := cluster.initialize(suiteContext)
		if err != nil {
			log.WithError(err).Fatal("Failed to initialize cluster")
		}
		log.DeferExitHandler(func() {
			cluster.cleanup(suiteContext)
		})
	})

	ctx.AfterSuite(func() {
		cluster.cleanup(suiteContext)
		cancel()
	})
}

func InitializeScenario(ctx *godog.ScenarioContext) {
	scenarioCtx, cancel := context.WithCancel(context.Background())
	log.DeferExitHandler(cancel)

	var kubectl kubernetesControl
	var pods podsManager
	ctx.BeforeScenario(func(*messages.Pickle) {
		kubectl = cluster.Kubectl().WithNamespace(scenarioCtx, "")
		if kubectl.Namespace != "" {
			log.Debugf("Running scenario in namespace: %s", kubectl.Namespace)
		}
		pods.kubectl = kubectl
		pods.ctx = scenarioCtx
		log.DeferExitHandler(func() { kubectl.Cleanup(scenarioCtx) })
	})
	ctx.AfterScenario(func(*messages.Pickle, error) {
		kubectl.Cleanup(scenarioCtx)
		cancel()
	})

	ctx.Step(`^"([^"]*)" have passed$`, func(d string) error { return waitDuration(scenarioCtx, d) })

	ctx.Step(`^"([^"]*)" is ([a-z]*)$`, func(name, state string) error {
		return pods.resourceIs(name, state)
	})
	ctx.Step(`^"([^"]*)" is ([a-z]*) with "([^"]*)"$`, func(name, state, option string) error {
		return pods.resourceIs(name, state, option)
	})

	ctx.Step(`^"([^"]*)" collects events with "([^"]*:[^"]*)"$`, pods.collectsEventsWith)
	ctx.Step(`^"([^"]*)" does not collect events with "([^"]*)" during "([^"]*)"$`, pods.doesNotCollectEvents)
	ctx.Step(`^an ephemeral container is started in "([^"]*)"$`, pods.startEphemeralContainerIn)
}