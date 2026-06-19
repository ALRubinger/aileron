package capture

import (
	"reflect"
	"strings"
	"testing"
)

// validDescriptorYAML is a minimal well-formed capture descriptor used as
// the base for the negative tests below (each mutates one field).
const validDescriptorYAML = `version: v1
name: gh
image: ""
container_name: aileron-auth-github
login_cmd: [gh, auth, login, --web]
token_cmd: [gh, auth, token]
browser_shim: echo
config_dir: ""
store_at: user/github
kind: user
`

func TestParseCaptureDescriptor_ValidRoundTrip(t *testing.T) {
	d, err := ParseCaptureDescriptor([]byte(validDescriptorYAML))
	if err != nil {
		t.Fatalf("ParseCaptureDescriptor: %v", err)
	}
	if d.Version != CaptureSchemaVersion {
		t.Errorf("Version = %q, want %q", d.Version, CaptureSchemaVersion)
	}
	if d.Name != "gh" {
		t.Errorf("Name = %q, want gh", d.Name)
	}
	if d.ContainerName != "aileron-auth-github" {
		t.Errorf("ContainerName = %q", d.ContainerName)
	}
	if want := []string{"gh", "auth", "login", "--web"}; !reflect.DeepEqual(d.LoginCmd, want) {
		t.Errorf("LoginCmd = %v, want %v", d.LoginCmd, want)
	}
	if want := []string{"gh", "auth", "token"}; !reflect.DeepEqual(d.TokenCmd, want) {
		t.Errorf("TokenCmd = %v, want %v", d.TokenCmd, want)
	}
	if d.BrowserShim != "echo" {
		t.Errorf("BrowserShim = %q, want echo", d.BrowserShim)
	}
	if d.ConfigDir != "" {
		t.Errorf("ConfigDir = %q, want empty", d.ConfigDir)
	}
	if d.Image != "" {
		t.Errorf("Image = %q, want empty", d.Image)
	}
	if d.StoreAt != "user/github" {
		t.Errorf("StoreAt = %q", d.StoreAt)
	}
	if d.Kind != "user" {
		t.Errorf("Kind = %q", d.Kind)
	}
}

func TestParseCaptureDescriptor_UnknownKeyRejected(t *testing.T) {
	y := validDescriptorYAML + "bogus_field: nope\n"
	if _, err := ParseCaptureDescriptor([]byte(y)); err == nil {
		t.Fatal("expected an error for an unknown YAML key (strict decode)")
	}
}

func TestParseCaptureDescriptor_WrongVersionRejected(t *testing.T) {
	y := strings.Replace(validDescriptorYAML, "version: v1", "version: v2", 1)
	_, err := ParseCaptureDescriptor([]byte(y))
	if err == nil {
		t.Fatal("expected an error for an unsupported version")
	}
	if !strings.Contains(err.Error(), "unsupported descriptor version") {
		t.Errorf("err = %v, want unsupported-version context", err)
	}
}

func TestParseCaptureDescriptor_MalformedYAMLRejected(t *testing.T) {
	if _, err := ParseCaptureDescriptor([]byte("this: : not: yaml")); err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
}

func TestParseCaptureDescriptor_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"empty name", func(s string) string { return strings.Replace(s, "name: gh", "name: \"\"", 1) }, "name"},
		{"empty container_name", func(s string) string {
			return strings.Replace(s, "container_name: aileron-auth-github", "container_name: \"\"", 1)
		}, "container_name"},
		{"empty login_cmd", func(s string) string {
			return strings.Replace(s, "login_cmd: [gh, auth, login, --web]", "login_cmd: []", 1)
		}, "login_cmd"},
		{"empty token_cmd", func(s string) string {
			return strings.Replace(s, "token_cmd: [gh, auth, token]", "token_cmd: []", 1)
		}, "token_cmd"},
		{"empty store_at", func(s string) string {
			return strings.Replace(s, "store_at: user/github", "store_at: \"\"", 1)
		}, "store_at"},
		{"empty kind", func(s string) string { return strings.Replace(s, "kind: user", "kind: \"\"", 1) }, "kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := tc.mutate(validDescriptorYAML)
			_, err := ParseCaptureDescriptor([]byte(y))
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want mention of %q", err, tc.wantSub)
			}
		})
	}
}

func TestParseCaptureDescriptor_EmptyCmdElementRejected(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"empty login_cmd element", func(s string) string {
			return strings.Replace(s, "login_cmd: [gh, auth, login, --web]", `login_cmd: [gh, "", login]`, 1)
		}, "login_cmd"},
		{"empty token_cmd element", func(s string) string {
			return strings.Replace(s, "token_cmd: [gh, auth, token]", `token_cmd: [gh, ""]`, 1)
		}, "token_cmd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := tc.mutate(validDescriptorYAML)
			_, err := ParseCaptureDescriptor([]byte(y))
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want mention of %q", err, tc.wantSub)
			}
		})
	}
}

func TestApply_NilStorePanics(t *testing.T) {
	d, err := ParseCaptureDescriptor([]byte(validDescriptorYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("Apply with a nil store should panic; the store is documented as required")
		}
	}()
	d.Apply(&Driver{}, "img", nil)
}

// TestApply_MapsEveryFieldOntoDriver proves the descriptor->driver adapter
// binds each field onto a real *Driver and that an omitted config_dir maps
// to an empty ConfigDirEnv.
func TestApply_MapsEveryFieldOntoDriver(t *testing.T) {
	d, err := ParseCaptureDescriptor([]byte(validDescriptorYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	drv := &Driver{}
	store := (&recordingStore{}).fn()
	d.Apply(drv, "ghcr.io/example/img:tag", store)

	if drv.Image != "ghcr.io/example/img:tag" {
		t.Errorf("Image = %q, want the caller-resolved image", drv.Image)
	}
	if drv.ContainerName != "aileron-auth-github" {
		t.Errorf("ContainerName = %q", drv.ContainerName)
	}
	if want := []string{"gh", "auth", "login", "--web"}; !reflect.DeepEqual(drv.LoginArgs, want) {
		t.Errorf("LoginArgs = %v, want %v", drv.LoginArgs, want)
	}
	if want := []string{"gh", "auth", "token"}; !reflect.DeepEqual(drv.TokenArgs, want) {
		t.Errorf("TokenArgs = %v, want %v", drv.TokenArgs, want)
	}
	if drv.BrowserShim != "echo" {
		t.Errorf("BrowserShim = %q, want echo", drv.BrowserShim)
	}
	if drv.ConfigDirEnv != "" {
		t.Errorf("ConfigDirEnv = %q, want empty when config_dir omitted", drv.ConfigDirEnv)
	}
	if drv.StoreAt != "user/github" {
		t.Errorf("StoreAt = %q", drv.StoreAt)
	}
	if drv.Kind != "user" {
		t.Errorf("Kind = %q", drv.Kind)
	}
	if drv.Store == nil {
		t.Error("Store was not bound")
	}
}

// TestApply_ConfigDirMapsThrough confirms the schema's general config_dir
// -> ConfigDirEnv mapping works for a tool that does set it (gh does not).
func TestApply_ConfigDirMapsThrough(t *testing.T) {
	y := strings.Replace(validDescriptorYAML, `config_dir: ""`, `config_dir: GH_CONFIG_DIR=/cfg`, 1)
	d, err := ParseCaptureDescriptor([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	drv := &Driver{}
	d.Apply(drv, "img", (&recordingStore{}).fn())
	if drv.ConfigDirEnv != "GH_CONFIG_DIR=/cfg" {
		t.Errorf("ConfigDirEnv = %q, want the config_dir token", drv.ConfigDirEnv)
	}
}

// TestApply_DescriptorImageUsedWhenCallerImageEmpty confirms the
// descriptor's own Image is the fallback when the caller passes no image.
func TestApply_DescriptorImageUsedWhenCallerImageEmpty(t *testing.T) {
	y := strings.Replace(validDescriptorYAML, `image: ""`, `image: ghcr.io/desc/default:tag`, 1)
	d, err := ParseCaptureDescriptor([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	drv := &Driver{}
	d.Apply(drv, "", (&recordingStore{}).fn())
	if drv.Image != "ghcr.io/desc/default:tag" {
		t.Errorf("Image = %q, want the descriptor default when caller image empty", drv.Image)
	}
}

// TestApply_CopiesCmdSlices ensures Apply does not alias the descriptor's
// backing arrays, so mutating the Driver's args never corrupts a cached
// descriptor.
func TestApply_CopiesCmdSlices(t *testing.T) {
	d, err := ParseCaptureDescriptor([]byte(validDescriptorYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	drv := &Driver{}
	d.Apply(drv, "img", (&recordingStore{}).fn())
	drv.LoginArgs[0] = "MUTATED"
	if d.LoginCmd[0] != "gh" {
		t.Errorf("descriptor LoginCmd was aliased and mutated: %v", d.LoginCmd)
	}
}
