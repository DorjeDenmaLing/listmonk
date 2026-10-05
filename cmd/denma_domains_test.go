package main

// The sending domains' checks (cmd/denma_domains.go). main's init() loads a
// config and the bundled files, so in dev's container: go test -c -o
// /tmp/cmd.test ./cmd, stuffbin it as docker/start.sh does the server, then
// run it in a folder with config.toml (/config/ddl.toml) -test.run TestDenma.

import "testing"

func TestDenmaMergeSPF(t *testing.T) {
	for in, want := range map[string]string{
		"v=spf1 include:_spf.google.com ~all": "v=spf1 include:_spf.google.com include:amazonses.com ~all",
		"v=spf1 mx -all":                      "v=spf1 mx include:amazonses.com -all",
		"v=spf1 a ?ALL":                       "v=spf1 a include:amazonses.com ?ALL",
		"v=spf1 mx":                           "v=spf1 mx include:amazonses.com",
	} {
		if got := denmaMergeSPF(in); got != want {
			t.Errorf("denmaMergeSPF(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDenmaSenderDomain(t *testing.T) {
	for in, want := range map[string]string{
		`"Halifax" <Programs@Shambhala.ORG>`: "shambhala.org",
		"office@kcl.example":                 "kcl.example",
		"not an address":                     "",
		"":                                   "",
	} {
		if got := denmaSenderDomain(in); got != want {
			t.Errorf("denmaSenderDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDenmaCheckMailFrom(t *testing.T) {
	ok := []string{"bounce.example.org", "a.b.example.org"}
	bad := []string{"example.org", "bounce.other.org", "bounceexample.org", "bad_name.example.org"}
	for _, m := range ok {
		if err := denmaCheckMailFrom(m, "example.org"); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
	for _, m := range bad {
		if denmaCheckMailFrom(m, "example.org") == nil {
			t.Errorf("%s: accepted", m)
		}
	}
}

func TestDenmaSendingVerdict(t *testing.T) {
	verified := denmaSESIdentity{Exists: true, Verified: true, DKIM: "SUCCESS", MailFromStatus: "SUCCESS"}
	ok := []denmaDNSRecord{{Status: "ok"}}
	for _, c := range []struct {
		name string
		st   denmaDomainState
		want string
		can  bool
	}{
		{"ready", denmaDomainState{SES: verified, Records: ok}, "ready", true},
		{"a record missing", denmaDomainState{SES: verified, Records: []denmaDNSRecord{{Status: "missing"}}}, "incomplete", true},
		{"an issue", denmaDomainState{SES: verified, Records: ok, Issues: []denmaDomainIssue{{Text: "x"}}}, "incomplete", true},
		{"mail from pending", denmaDomainState{SES: denmaSESIdentity{Exists: true, Verified: true, DKIM: "SUCCESS", MailFromStatus: "PENDING"}}, "incomplete", true},
		{"pending", denmaDomainState{SES: denmaSESIdentity{Exists: true, DKIM: "PENDING"}}, "waiting", false},
		{"failed", denmaDomainState{SES: denmaSESIdentity{Exists: true, DKIM: "FAILED"}}, "failed", false},
		{"verified without DKIM", denmaDomainState{SES: denmaSESIdentity{Exists: true, Verified: true, DKIM: "PENDING"}}, "waiting", false},
		{"not in SES", denmaDomainState{}, "missing", false},
		{"unreachable", denmaDomainState{Error: "timeout"}, "error", false},
	} {
		denmaSendingVerdict(&c.st)
		if c.st.Status != c.want || c.st.CanSend != c.can {
			t.Errorf("%s: %s (can send %v), want %s (%v)", c.name, c.st.Status, c.st.CanSend, c.want, c.can)
		}
	}
}

func TestDenmaIssues(t *testing.T) {
	ref := denmaDomainRef{Domain: "hub.org", Topic: "arn:topic", ConfigSet: "set"}
	fixes := func(id denmaSESIdentity, ref denmaDomainRef) []string {
		var out []string
		for _, i := range denmaIssues(id, ref) {
			out = append(out, i.Fix)
		}
		return out
	}
	done := denmaSESIdentity{Exists: true, DKIM: "SUCCESS", DKIMOrigin: "AWS_SES", MailFrom: "bounce.x.org",
		MailFromStatus: "SUCCESS", BounceTopic: "arn:topic", ComplaintTopic: "arn:topic", Headers: true, ConfigSet: "set"}
	if f := fixes(done, ref); len(f) != 0 {
		t.Errorf("set up: %v", f)
	}
	if f := fixes(denmaSESIdentity{}, ref); len(f) != 1 || f[0] != "setup" {
		t.Errorf("not in SES: %v", f)
	}
	failed := done
	failed.DKIM, failed.MailFromStatus, failed.Headers = "FAILED", "FAILED", false
	if f := fixes(failed, ref); len(f) != 3 || f[0] != "dkim" || f[1] != "mail_from" || f[2] != "notifications" {
		t.Errorf("failed: %v", f)
	}
	// DKIM keys SES doesn't manage: no fix of ours.
	byod := done
	byod.DKIMOrigin, byod.DKIM = "EXTERNAL", "FAILED"
	if is := denmaIssues(byod, ref); len(is) != 1 || is[0].Fix != "" {
		t.Errorf("BYODKIM: %v", is)
	}
	// Without the hub's settings to copy, nothing to compare.
	if f := fixes(denmaSESIdentity{Exists: true, DKIM: "SUCCESS", MailFrom: "b.x.org"}, denmaDomainRef{Error: "none"}); len(f) != 0 {
		t.Errorf("no reference: %v", f)
	}
}
