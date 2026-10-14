package fingerprint

import "testing"

func TestHashStable(t *testing.T) {
	h := Host{OS: "linux", Architecture: "amd64", KernelRelease: "test", CgroupMode: "v2"}
	if Hash(h) != Hash(h) {
		t.Fatal("fingerprint hash is not stable")
	}
}
