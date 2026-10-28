/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"regexp"
	"strings"
	"testing"
)

func TestControllerListenPortsMatchFlags(t *testing.T) {
	out, err := renderWithValuesFile(t, writeSecretAccessValues(t, "controller:\n  mcp:\n    enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	for flagName, portName := range map[string]string{
		"callback-addr": "callback", "webhook-trigger-addr": "webhook-trigger", "mcp-addr": "mcp",
	} {
		flag := regexp.MustCompile(`--` + flagName + `=\S*:(\d+)`).FindStringSubmatch(out)
		port := regexp.MustCompile(`containerPort: (\d+)\n\s+name: ` + portName + `\n`).FindStringSubmatch(out)
		if flag == nil || port == nil || flag[1] != port[1] {
			t.Errorf("--%s %v does not match the %s containerPort %v", flagName, flag, portName, port)
		}
	}
}

func TestControllerListenPortClashFails(t *testing.T) {
	for name, values := range map[string]string{
		"mcp and callback":     "controller:\n  mcp:\n    enabled: true\n    addr: \":8084\"\n",
		"host form":            "controller:\n  mcp:\n    enabled: true\n  callback:\n    addr: \"0.0.0.0:8090\"\n",
		"callback and trigger": "controller:\n  callback:\n    addr: \":8083\"\n",
		"metrics and callback": "controller:\n  metrics:\n    port: 8084\n",
		"probe arg and callback": "controller:\n  args:\n    - --leader-elect\n" +
			"    - --health-probe-bind-address=:8084\n",
		"probe split arg and callback": "controller:\n  args:\n    - --health-probe-bind-address\n" +
			"    - \":8084\"\n",
		"single-dash probe arg and callback": "controller:\n  args:\n    - -health-probe-bind-address=:8084\n",
		"single-dash probe split arg and callback": "controller:\n  args:\n    - -health-probe-bind-address\n" +
			"    - \":8084\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := renderWithValuesFile(t, writeSecretAccessValues(t, values))
			if err == nil || !strings.Contains(err.Error(), "must bind distinct ports") {
				t.Errorf("want a distinct-ports failure, got %v", err)
			}
		})
	}
}

func TestControllerDisabledListenerNotCounted(t *testing.T) {
	values := "controller:\n  webhookTrigger:\n    enabled: false\n  callback:\n    addr: \":8083\"\n"
	if _, err := renderWithValuesFile(t, writeSecretAccessValues(t, values)); err != nil {
		t.Fatal(err)
	}
}

func TestControllerMovedProbeFreesDefaultPort(t *testing.T) {
	for name, args := range map[string]string{
		"double dash":       "    - --health-probe-bind-address=:9000\n",
		"single dash":       "    - -health-probe-bind-address=:9000\n",
		"single dash split": "    - -health-probe-bind-address\n    - \":9000\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			values := "controller:\n  args:\n    - --leader-elect\n" + args +
				"  livenessProbe:\n    httpGet:\n      port: 9000\n  readinessProbe:\n    httpGet:\n      port: 9000\n" +
				"  callback:\n    addr: \":8081\"\n"
			if _, err := renderWithValuesFile(t, writeSecretAccessValues(t, values)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
