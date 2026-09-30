package cc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Byte-exact port of the device fingerprint in proxy.mjs (which is itself
// aligned line-by-line with the official CLI's buildMachineFingerprint).
//
// Signal values are derived deterministically from the API key, so one key
// always reports the same device across restarts and instances; the
// fingerprintSalt rotates which device every key maps to.

// fpSalt is the CLI's root salt (buildMachineFingerprint constant sb).
const fpSalt = "command-code:device-fingerprint:v1"

type fpCPU struct {
	model string
	cores int
}

var fpCPUs = []fpCPU{
	{"12th Gen Intel(R) Core(TM) i7-12650H", 10},
	{"12th Gen Intel(R) Core(TM) i5-12400F", 6},
	{"12th Gen Intel(R) Core(TM) i9-12900K", 16},
	{"13th Gen Intel(R) Core(TM) i7-13700K", 16},
	{"13th Gen Intel(R) Core(TM) i5-13600K", 14},
	{"13th Gen Intel(R) Core(TM) i9-13900K", 24},
	{"Intel(R) Core(TM) Ultra 7 155H", 16},
	{"Intel(R) Core(TM) Ultra 9 285H", 16},
	{"Intel(R) Core(TM) i9-14900K", 24},
	{"Intel(R) Core(TM) i7-14700K", 20},
	{"AMD Ryzen 7 7800X3D", 8},
	{"AMD Ryzen 9 7950X", 16},
	{"AMD Ryzen 5 7600", 6},
	{"AMD Ryzen 9 7900X", 12},
	{"AMD Ryzen 7 5800X3D", 8},
}

var fpMems = []int{8, 16, 24, 32, 48, 64}

var fpTZs = []string{
	"America/New_York", "America/Chicago", "America/Los_Angeles", "America/Toronto",
	"Europe/London", "Europe/Berlin", "Europe/Paris", "Europe/Moscow",
	"Asia/Shanghai", "Asia/Tokyo", "Asia/Singapore", "Asia/Seoul", "Asia/Hong_Kong",
	"Australia/Sydney", "Pacific/Auckland",
}

var fpMACCounts = []int{2, 3, 4, 5}

var fpOSUsers = []string{"dev", "user", "admin", "coder", "engineer", "work"}
var fpMailDomains = []string{"gmail.com", "outlook.com", "qq.com", "163.com"}

// DeviceProfile is the single source of device truth shared by the
// fingerprint, config.environment, config.workingDir, x-project-slug and the
// lifecycle os field — so they can never contradict each other or leak the
// host's real platform.
type DeviceProfile struct {
	Platform   string // "win32"
	Arch       string // "x64"
	OSRelease  string
	ProjectDir string
}

func NewDeviceProfile(projectDirOverride string) DeviceProfile {
	dir := projectDirOverride
	if dir == "" {
		dir = `C:\Users\dev\projects\app`
	}
	return DeviceProfile{Platform: "win32", Arch: "x64", OSRelease: "10.0.22631", ProjectDir: dir}
}

// Fingerprint is the wire body of POST /alpha/fingerprint/record.
type Fingerprint struct {
	Thumbmark  string             `json:"thumbmark"`
	Components FingerprintSignals `json:"components"`
}

type FingerprintSignals struct {
	MachineIDHash string   `json:"machineIdHash,omitempty"`
	MACHashes     []string `json:"macHashes,omitempty"`
	OSUserHash    string   `json:"osUserHash,omitempty"`
	HostnameHash  string   `json:"hostnameHash,omitempty"`
	GitEmailHash  string   `json:"gitEmailHash,omitempty"`
	Platform      string   `json:"platform"`
	Arch          string   `json:"arch"`
	OSRelease     string   `json:"osRelease"`
	CPUModel      string   `json:"cpuModel"`
	CPUCount      int      `json:"cpuCount"`
	MemGiB        int      `json:"memGiB"`
	IsContainer   bool     `json:"isContainer"`
	Timezone      string   `json:"timezone"`
	Runtime       string   `json:"runtime"`
	CollectorVer  int      `json:"collectorVersion"`
}

// Fingerprinter derives per-key device identities.
type Fingerprinter struct {
	salt   string
	device DeviceProfile
}

func NewFingerprinter(salt string, device DeviceProfile) *Fingerprinter {
	return &Fingerprinter{salt: salt, device: device}
}

func (f *Fingerprinter) Device() DeviceProfile { return f.device }

// fpDigest = sha256(salt \0 apiKey \0 field), binary digest.
// The user salt only influences WHICH device is faked; the hash stage always
// uses the CLI's fixed salt (fingerprintHash below).
func (f *Fingerprinter) digest(apiKey, field string) [sha256.Size]byte {
	var buf strings.Builder
	buf.WriteString(f.salt)
	buf.WriteByte(0)
	buf.WriteString(apiKey)
	buf.WriteByte(0)
	buf.WriteString(field)
	return sha256.Sum256([]byte(buf.String()))
}

// fpPickIndex deterministically selects from a candidate pool by scoring
// each item with digest(field \0 label) and picking the bytewise maximum.
// Adding candidates only re-maps keys whose new candidate happens to win —
// unlike modulo, pool growth doesn't reshuffle every key.
func (f *Fingerprinter) pickIndex(apiKey, field string, labels []string) int {
	bestIdx := 0
	var best []byte
	for i, label := range labels {
		d := f.digest(apiKey, field+"\x00"+label)
		if best == nil || bytes.Compare(d[:], best) > 0 {
			best = append(best[:0:0], d[:]...)
			bestIdx = i
		}
	}
	return bestIdx
}

func (f *Fingerprinter) hex(apiKey, field string, n int) string {
	d := f.digest(apiKey, field)
	return hex.EncodeToString(d[:n])
}

// fingerprintHash is the CLI's hashSignal:
// sha256(fpSalt \0 lowercased-trimmed-value) hex; empty value → omitted field.
func fingerprintHash(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(fpSalt + "\x00" + strings.ToLower(v)))
	return hex.EncodeToString(sum[:])
}

// Generate builds the deterministic fingerprint for an API key.
func (f *Fingerprinter) Generate(apiKey string) Fingerprint {
	cpuLabels := make([]string, len(fpCPUs))
	for i, c := range fpCPUs {
		cpuLabels[i] = fmt.Sprintf("%s|%d", c.model, c.cores)
	}
	cpu := fpCPUs[f.pickIndex(apiKey, "cpu", cpuLabels)]

	memLabels := make([]string, len(fpMems))
	for i, m := range fpMems {
		memLabels[i] = fmt.Sprintf("%d", m)
	}
	memGiB := fpMems[f.pickIndex(apiKey, "mem", memLabels)]

	tz := fpTZs[f.pickIndex(apiKey, "timezone", fpTZs)]

	macCountLabels := make([]string, len(fpMACCounts))
	for i, m := range fpMACCounts {
		macCountLabels[i] = fmt.Sprintf("%d", m)
	}
	macCount := fpMACCounts[f.pickIndex(apiKey, "macCount", macCountLabels)]

	osUser := fpOSUsers[f.pickIndex(apiKey, "osUser", fpOSUsers)]
	mailDomain := fpMailDomains[f.pickIndex(apiKey, "mailDomain", fpMailDomains)]

	// Windows MachineGuid shape: 8-4-4-4-12.
	mid := f.hex(apiKey, "machineId", 16)
	machineID := fmt.Sprintf("%s-%s-%s-%s-%s", mid[0:8], mid[8:12], mid[12:16], mid[16:20], mid[20:32])

	macs := make([]string, macCount)
	for i := 0; i < macCount; i++ {
		d := f.digest(apiKey, fmt.Sprintf("mac%d", i))
		b := d[:6]
		macs[i] = fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
	}
	sort.Strings(macs) // CLI dedupes then sorts MACs

	hostname := "DESKTOP-" + strings.ToUpper(f.hex(apiKey, "hostname", 4))
	gitEmail := fmt.Sprintf("%s.%s@%s", osUser, f.hex(apiKey, "gitEmail", 3), mailDomain)

	macHashes := make([]string, 0, len(macs))
	for _, m := range macs {
		if h := fingerprintHash(m); h != "" {
			macHashes = append(macHashes, h)
		}
	}

	// CLI thumbmark: fpSalt \0 "machine" \0 join([machineId, macs], '|').
	// (hostname/cpuModel only join in when machineId is empty — never here.)
	thumbSeed := machineID + "|" + strings.Join(macs, ",")
	if thumbSeed == "" {
		thumbSeed = "unknown"
	}
	thumb := sha256.Sum256([]byte(fpSalt + "\x00machine\x00" + thumbSeed))

	return Fingerprint{
		Thumbmark: hex.EncodeToString(thumb[:]),
		Components: FingerprintSignals{
			MachineIDHash: fingerprintHash(machineID),
			MACHashes:     macHashes,
			OSUserHash:    fingerprintHash(osUser),
			HostnameHash:  fingerprintHash(hostname),
			GitEmailHash:  fingerprintHash(gitEmail),
			Platform:      f.device.Platform,
			Arch:          f.device.Arch,
			OSRelease:     f.device.OSRelease,
			CPUModel:      cpu.model,
			CPUCount:      cpu.cores,
			MemGiB:        memGiB,
			IsContainer:   false,
			Timezone:      tz,
			Runtime:       "cli",
			CollectorVer:  1,
		},
	}
}

// SlugifyProjectPath mirrors the CLI's slugify(workingDir): lowercase,
// non-alphanumerics → '-', trim '-', empty → "root".
func SlugifyProjectPath(p string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(p) {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlnum {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "root"
	}
	return s
}
