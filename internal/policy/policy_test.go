package policy

import (
	"testing"

	"github.com/hypervisor-io/punk-records/internal/spec"
)

func testBundle(t *testing.T) *spec.Bundle {
	t.Helper()
	p, err := spec.ParsePolicy("default.yaml", []byte(`
name: default
action_classes:
  - name: read
    min_autonomy: observe
    tool_patterns: ["susanoo__list_*", "susanoo__get_*"]
  - name: notify
    min_autonomy: propose
    tool_patterns: ["susanoo__add_incident_comment"]
  - name: mutate
    min_autonomy: auto
    tool_patterns: ["*__execute_*"]
`))
	if err != nil {
		t.Fatal(err)
	}
	return &spec.Bundle{Policies: map[string]*spec.PolicySpec{"default": p}}
}

func TestCheckLadder(t *testing.T) {
	e := FromBundle(testBundle(t))
	cases := []struct {
		autonomy, tool string
		want           Verdict
	}{
		{"observe", "susanoo__get_incident", Allow},
		{"observe", "susanoo__add_incident_comment", Deny},  // notify needs propose
		{"advise", "susanoo__add_incident_comment", Deny},   // still below
		{"propose", "susanoo__add_incident_comment", Allow}, // at level
		{"propose", "runbooks__execute_restart", Propose},   // auto class, propose agent
		{"advise", "runbooks__execute_restart", Deny},       // below propose
		{"auto", "runbooks__execute_restart", Allow},
		{"propose", "totally__unknown_tool", Propose}, // unclassified: conservative
	}
	for _, c := range cases {
		got := e.Check(c.autonomy, c.tool)
		if got.Verdict != c.want {
			t.Errorf("Check(%s, %s) = %s (%s), want %s", c.autonomy, c.tool, got.Verdict, got.Reason, c.want)
		}
	}
}

func TestPermissiveWithoutPolicies(t *testing.T) {
	e := FromBundle(&spec.Bundle{Policies: map[string]*spec.PolicySpec{}})
	if d := e.Check("observe", "anything__at_all"); d.Verdict != Allow {
		t.Fatalf("policy-less deployment must allow, got %s", d.Verdict)
	}
}

func TestClassFor(t *testing.T) {
	e := FromBundle(testBundle(t))
	name, minAut, ok := e.ClassFor("susanoo__list_incidents")
	if !ok || name != "read" || minAut != "observe" {
		t.Fatalf("ClassFor = %s/%s/%v", name, minAut, ok)
	}
	if _, _, ok := e.ClassFor("nope__nothing"); ok {
		t.Fatal("unmatched tool classified")
	}
}

func TestMemoryACL(t *testing.T) {
	p, err := spec.ParsePolicy("acl.yaml", []byte(`
name: acl
action_classes:
  - {name: read, min_autonomy: observe, tool_patterns: ["x__*"]}
memory_acl:
  - prefix: /reference
    read_only: true
  - prefix: /svc/db
    writers: [database]
  - prefix: /svc
    writers: ["*"]
`))
	if err != nil {
		t.Fatal(err)
	}
	e := FromBundle(&spec.Bundle{Policies: map[string]*spec.PolicySpec{"acl": p}})

	cases := []struct {
		agent, key string
		want       Verdict
	}{
		{"database", "/reference/runbook", Deny}, // read-only wins
		{"database", "/svc/db/failover", Allow},  // named writer
		{"network", "/svc/db/failover", Deny},    // longest prefix: db-only
		{"network", "/svc/net/edge", Allow},      // wildcard rule
		{"anyone", "/unruled/key", Allow},        // no rule
	}
	for _, c := range cases {
		if got := e.CheckMemoryWrite(c.agent, c.key); got.Verdict != c.want {
			t.Errorf("CheckMemoryWrite(%s,%s) = %s (%s), want %s", c.agent, c.key, got.Verdict, got.Reason, c.want)
		}
	}
}
