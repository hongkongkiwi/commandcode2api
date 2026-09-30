// Golden-vector test for the fingerprint port: the Go implementation must
// produce byte-identical output to the Node reference (proxy.mjs
// generateFingerprint) for the same keys and salts. Vectors were generated
// by running the reference functions under Node.
package cc

import (
	"encoding/json"
	"os"
	"testing"
)

type goldenVector struct {
	Thumbmark     string   `json:"thumbmark"`
	MachineIDHash string   `json:"machineIdHash"`
	MACHashes     []string `json:"macHashes"`
	OSUserHash    string   `json:"osUserHash"`
	HostnameHash  string   `json:"hostnameHash"`
	GitEmailHash  string   `json:"gitEmailHash"`
	CPUModel      string   `json:"cpuModel"`
	CPUCount      int      `json:"cpuCount"`
	MemGiB        int      `json:"memGiB"`
	Timezone      string   `json:"timezone"`
}

func TestFingerprintGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/fingerprint_golden.json")
	if err != nil {
		t.Fatalf("golden fixture missing: %v", err)
	}
	var vectors map[string]goldenVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}

	for spec, want := range vectors {
		var apiKey, salt string
		if idx := indexOf(spec, "||"); idx >= 0 {
			salt, apiKey = spec[:idx], spec[idx+2:]
		} else {
			apiKey = spec
		}
		f := NewFingerprinter(salt, NewDeviceProfile(""))
		got := f.Generate(apiKey)

		if got.Thumbmark != want.Thumbmark {
			t.Errorf("%s: thumbmark\n got %s\nwant %s", spec, got.Thumbmark, want.Thumbmark)
		}
		c := got.Components
		if c.MachineIDHash != want.MachineIDHash {
			t.Errorf("%s: machineIdHash\n got %s\nwant %s", spec, c.MachineIDHash, want.MachineIDHash)
		}
		if len(c.MACHashes) != len(want.MACHashes) {
			t.Errorf("%s: macHashes count %d, want %d", spec, len(c.MACHashes), len(want.MACHashes))
		} else {
			for i := range c.MACHashes {
				if c.MACHashes[i] != want.MACHashes[i] {
					t.Errorf("%s: macHashes[%d]\n got %s\nwant %s", spec, i, c.MACHashes[i], want.MACHashes[i])
				}
			}
		}
		if c.OSUserHash != want.OSUserHash {
			t.Errorf("%s: osUserHash mismatch", spec)
		}
		if c.HostnameHash != want.HostnameHash {
			t.Errorf("%s: hostnameHash mismatch", spec)
		}
		if c.GitEmailHash != want.GitEmailHash {
			t.Errorf("%s: gitEmailHash mismatch", spec)
		}
		if c.CPUModel != want.CPUModel || c.CPUCount != want.CPUCount {
			t.Errorf("%s: cpu = %s/%d, want %s/%d", spec, c.CPUModel, c.CPUCount, want.CPUModel, want.CPUCount)
		}
		if c.MemGiB != want.MemGiB {
			t.Errorf("%s: memGiB = %d, want %d", spec, c.MemGiB, want.MemGiB)
		}
		if c.Timezone != want.Timezone {
			t.Errorf("%s: timezone = %s, want %s", spec, c.Timezone, want.Timezone)
		}
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Determinism: same key → same fingerprint across calls (device identity
// must survive restarts).
func TestFingerprintDeterministic(t *testing.T) {
	f := NewFingerprinter("", NewDeviceProfile(""))
	a := f.Generate("user_stability")
	b := f.Generate("user_stability")
	if a.Thumbmark != b.Thumbmark || a.Components.MachineIDHash != b.Components.MachineIDHash {
		t.Fatal("fingerprint must be deterministic per key")
	}
	// Different keys → different devices (with overwhelming probability).
	c := f.Generate("user_other")
	if c.Thumbmark == a.Thumbmark {
		t.Fatal("different keys must map to different fingerprints")
	}
}

// Slug: CLI slugify(workingDir), including the windows default.
func TestSlugifyProjectPath(t *testing.T) {
	cases := map[string]string{
		`C:\Users\dev\projects\app`: "c-users-dev-projects-app",
		"/home/dev/My Proj":         "home-dev-my-proj",
		"":                          "root",
		"--weird--path--":           "weird-path",
	}
	for in, want := range cases {
		if got := SlugifyProjectPath(in); got != want {
			t.Errorf("SlugifyProjectPath(%q) = %q, want %q", in, got, want)
		}
	}
}
