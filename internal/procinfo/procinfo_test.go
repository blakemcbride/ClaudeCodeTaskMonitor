package procinfo

import (
	"os"
	"testing"
	"time"
)

func TestAlive(t *testing.T) {
	self := os.Getpid()
	if !Alive(self, time.Now().UnixMilli()) {
		t.Fatal("this process should count as alive")
	}
	// A session file older than the process holding its pid means the pid
	// was reused.
	if Alive(self, time.Now().Add(-24*time.Hour).UnixMilli()) {
		t.Fatal("a reused pid was taken for the original session")
	}
	if Alive(1<<30, 0) {
		t.Fatal("a pid that cannot exist counted as alive")
	}
}

func TestSample(t *testing.T) {
	st := NewSampler().Sample([]int{os.Getpid()})
	if st[os.Getpid()].RSS == 0 {
		t.Fatal("no memory reported for this process")
	}
}
