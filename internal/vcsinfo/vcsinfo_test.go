package vcsinfo

import "testing"

func TestParseGit(t *testing.T) {
	out := "# branch.oid abc\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +2 -1\n1 .M N... 100644 100644 100644 a b file.go\n? new.txt\n"
	got := parseGit([]byte(out))
	want := Info{System: "git", Branch: "main", Upstream: "origin/main", Ahead: 2, Behind: 1, Dirty: 2}
	if *got != want {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
	if g := parseGit([]byte("# branch.head topic\n")); g.Upstream != "" || g.Dirty != 0 {
		t.Fatalf("no upstream: %+v", *g)
	}
}

func TestParseSVN(t *testing.T) {
	info := parseSVNInfo([]byte(`<?xml version="1.0"?><info><entry path="." revision="412" kind="dir">
<url>https://svn.example/repo/trunk/app</url><relative-url>^/trunk/app</relative-url></entry></info>`))
	if info == nil || info.System != "svn" || info.Branch != "^/trunk/app" || info.Revision != "412" {
		t.Fatalf("got %+v", info)
	}
	if parseSVNInfo([]byte("svn: E155007: not a working copy")) != nil {
		t.Fatal("an error message was taken for a working copy")
	}
	n := parseSVNStatus([]byte(`<?xml version="1.0"?><status><target path=".">
<entry path="a.c"><wc-status item="modified" props="none"/></entry>
<entry path="new.txt"><wc-status item="unversioned" props="none"/></entry>
<entry path="lib"><wc-status item="external" props="none"/></entry>
<entry path="b.c"><wc-status item="normal" props="modified"/></entry>
</target></status>`))
	if n != 2 {
		t.Fatalf("counted %d changed paths, want 2 (unversioned files are not counted)", n)
	}
}
