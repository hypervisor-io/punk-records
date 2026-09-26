package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMessagingDeliveryConfig(t *testing.T) {
	c := Default()
	if c.Messaging.MaxUnreadPerRecipient != 200 || c.Messaging.RetentionDays != 30 || c.Messaging.MemberExpiryDays != 7 {
		t.Fatalf("defaults = %+v", c.Messaging)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("messaging:\n  max_unread_per_recipient: 12\n  retention_days: 7\n  member_expiry_days: 14\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil || c.Messaging.MaxUnreadPerRecipient != 12 || c.Messaging.RetentionDays != 7 || c.Messaging.MemberExpiryDays != 14 {
		t.Fatalf("file = %+v %v", c, err)
	}
	t.Setenv("PUNK_MESSAGING_MAX_UNREAD_PER_RECIPIENT", "3")
	t.Setenv("PUNK_MESSAGING_RETENTION_DAYS", "0")
	t.Setenv("PUNK_MESSAGING_MEMBER_EXPIRY_DAYS", "0")
	c, err = Load(p)
	if err != nil || c.Messaging.MaxUnreadPerRecipient != 3 || c.Messaging.RetentionDays != 0 || c.Messaging.MemberExpiryDays != 0 {
		t.Fatalf("env = %+v %v", c, err)
	}
	for _, key := range []string{"PUNK_MESSAGING_MAX_UNREAD_PER_RECIPIENT", "PUNK_MESSAGING_RETENTION_DAYS", "PUNK_MESSAGING_MEMBER_EXPIRY_DAYS"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "-1")
			if _, err := Load(p); err == nil {
				t.Fatal("negative config accepted")
			}
		})
	}
}

func TestMessagingEnabledConfig(t *testing.T) {
	t.Setenv("PUNK_MESSAGING", "")
	if Default().Messaging.Enabled {
		t.Fatal("messaging enabled by default")
	}
	p := filepath.Join(t.TempDir(), "enabled.yaml")
	if err := os.WriteFile(p, []byte("messaging:\n  enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil || !c.Messaging.Enabled {
		t.Fatalf("file switch = %+v %v", c, err)
	}
	for _, tc := range []struct {
		env  string
		want bool
	}{{"0", false}, {"false", false}, {"1", true}, {"true", true}} {
		t.Setenv("PUNK_MESSAGING", tc.env)
		c, err := Load(p)
		if err != nil || c.Messaging.Enabled != tc.want {
			t.Fatalf("env %s = %+v %v", tc.env, c, err)
		}
	}
	t.Setenv("PUNK_MESSAGING", "invalid")
	if _, err := Load(p); err == nil {
		t.Fatal("invalid boolean accepted")
	}
}
