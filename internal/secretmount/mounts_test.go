/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package secretmount

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestValidateDataKey_RejectsInvalidKeys: every key OttoFlow mounts becomes a projected filename
// in the runner Job and a path under the mount directory in the runner, so a value that could not
// be a real Secret data key must be refused before it reaches either. The rejected set is what the
// API server refuses for a Secret data key: anything outside [-._a-zA-Z0-9], the two dot names,
// and a '..' prefix. Each rejection must name the key, because the caller wraps it into a message
// that points the operator at the field to fix.
func TestValidateDataKey_RejectsInvalidKeys(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"parent traversal", "../token"},
		{"deep traversal to the SA token", "../../../../var/run/secrets/kubernetes.io/serviceaccount/token"},
		{"dotdot prefix", "..hidden"},
		{"subdirectory", "sub/dir"},
		{"absolute path", "/abs"},
		{"backslash", `back\slash`},
		{"space", "with space"},
		{"colon", "colon:sep"},
		{"NUL byte", "nul\x00byte"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDataKey(tc.key)
			if err == nil {
				t.Fatalf("ValidateDataKey(%q) = nil, want an error", tc.key)
			}
			// The message quotes the key with %q, so compare against the quoted form: a
			// backslash or a control character is escaped there, not echoed raw.
			if tc.key != "" && !strings.Contains(err.Error(), strconv.Quote(tc.key)) {
				t.Errorf("error does not name the offending key %q: %v", tc.key, err)
			}
		})
	}
}

// TestValidateDataKey_AcceptsValidKeys: the guard must never refuse a key the API server would
// accept in a Secret, or a working configuration becomes unmountable. A '..' inside the key is
// fine; only the prefix is refused.
func TestValidateDataKey_AcceptsValidKeys(t *testing.T) {
	for _, key := range []string{"ca.crt", "tls.key", "token", "my-config.yaml", "kubeconfig", "A_b-c.d9", ".hidden", "a..b", "x"} {
		t.Run(key, func(t *testing.T) {
			if err := ValidateDataKey(key); err != nil {
				t.Fatalf("ValidateDataKey(%q) = %v, want nil", key, err)
			}
		})
	}
}

// TestKey pins the lookup-key shape both sides of the mount contract derive independently: the
// controller writes it into OTTOFLOW_SECRET_MOUNTS and the runner looks values up by it.
func TestKey(t *testing.T) {
	if got, want := Key("team-a", "creds", "token"), "team-a/creds/token"; got != want {
		t.Fatalf("Key() = %q, want %q", got, want)
	}
}

// TestMounts_ReadFile covers the runner-side read: a mounted key resolves to the file's bytes, an
// unmounted key is ErrNotFound, and a mounted key whose file is absent (an optional Secret that
// was not there) is ErrNotFound rather than an I/O failure.
func TestMounts_ReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := Mounts{
		Key("ns", "creds", "token"):   path,
		Key("ns", "creds", "missing"): filepath.Join(dir, "missing"),
	}

	got, err := m.ReadFile("ns", "creds", "token")
	if err != nil || string(got) != "s3cret" {
		t.Fatalf("ReadFile(mounted) = %q, %v; want \"s3cret\", nil", got, err)
	}
	if _, err := m.ReadFile("ns", "creds", "unmounted"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadFile(unmounted) = %v, want ErrNotFound", err)
	}
	if _, err := m.ReadFile("ns", "creds", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadFile(mounted, file absent) = %v, want ErrNotFound", err)
	}
}
