package launch

import (
	"testing"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
)

func TestSandboxCacheVolumesEmpty(t *testing.T) {
	mounts, err := sandboxCacheVolumes(nil)
	if err != nil {
		t.Fatalf("sandboxCacheVolumes(nil): %v", err)
	}
	if len(mounts) != 0 {
		t.Fatalf("mounts = %+v, want none", mounts)
	}
}

func TestSandboxCacheVolumesMapsDeclarations(t *testing.T) {
	paths := []sandboxcomposition.CachePath{
		{CLI: "npm", Identity: "ws-A", ContainerPath: "/home/agent/.npm"},
		{CLI: "pip", Identity: "ws-A", ContainerPath: "/home/agent/.cache/pip"},
	}
	mounts, err := sandboxCacheVolumes(paths)
	if err != nil {
		t.Fatalf("sandboxCacheVolumes: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mounts = %+v, want 2", mounts)
	}
	for i, m := range mounts {
		if !m.Named {
			t.Fatalf("mount %d is not Named: %+v", i, m)
		}
		if m.ReadOnly {
			t.Fatalf("cache mount %d must be read-write: %+v", i, m)
		}
	}
	wantFirst := sandboxcomposition.CacheVolumeName("npm", "ws-A")
	if mounts[0].Source != wantFirst {
		t.Fatalf("mounts[0].Source = %q, want %q", mounts[0].Source, wantFirst)
	}
	if mounts[0].Target != "/home/agent/.npm" {
		t.Fatalf("mounts[0].Target = %q", mounts[0].Target)
	}
}

func TestSandboxCacheVolumesRequiresCLI(t *testing.T) {
	_, err := sandboxCacheVolumes([]sandboxcomposition.CachePath{
		{Identity: "ws", ContainerPath: "/cache"},
	})
	if err == nil {
		t.Fatal("expected error for missing cli")
	}
}

func TestSandboxCacheVolumesRequiresContainerPath(t *testing.T) {
	_, err := sandboxCacheVolumes([]sandboxcomposition.CachePath{
		{CLI: "npm", Identity: "ws"},
	})
	if err == nil {
		t.Fatal("expected error for missing container_path")
	}
}

func TestSandboxCacheVolumesRejectsRelativeContainerPath(t *testing.T) {
	_, err := sandboxCacheVolumes([]sandboxcomposition.CachePath{
		{CLI: "npm", Identity: "ws", ContainerPath: "relative/cache"},
	})
	if err == nil {
		t.Fatal("expected error for relative container_path")
	}
}

func TestSandboxCacheVolumesEmptyIdentityAllowed(t *testing.T) {
	// Identity is optional; an empty identity is a valid (deterministic) key.
	mounts, err := sandboxCacheVolumes([]sandboxcomposition.CachePath{
		{CLI: "npm", ContainerPath: "/cache"},
	})
	if err != nil {
		t.Fatalf("sandboxCacheVolumes: %v", err)
	}
	if len(mounts) != 1 || mounts[0].Source != sandboxcomposition.CacheVolumeName("npm", "") {
		t.Fatalf("mounts = %+v", mounts)
	}
}
