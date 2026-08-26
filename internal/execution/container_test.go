package execution

import (
	"slices"
	"testing"
)

func TestContainerArgsEnforceInitialTrustBoundary(t *testing.T) {
	args, err := (ContainerRunner{}).BuildArgs(t.TempDir(), "example@sha256:abc", []string{"go", "test", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"none", "--read-only", "ALL", "no-new-privileges", "65532:65532", "HOME=/tmp/simpleton-home", "LANG=C.UTF-8", "TZ=UTC"} {
		if !slices.Contains(args, required) {
			t.Fatalf("container args omit %q: %#v", required, args)
		}
	}
	for _, value := range args {
		if value == "--privileged" || value == "host" || value == "--env-file" {
			t.Fatalf("unsafe container argument %q", value)
		}
	}
}
