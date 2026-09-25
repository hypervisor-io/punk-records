package main

import (
	"os"
	"strconv"
	"time"

	"github.com/hypervisor-io/punk-records/internal/config"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func configuredMessageRegion(db *store.DB, cfg *config.Config) *region.Store {
	r := region.New(db, nil)
	r.MaxUnreadPerRecipient = cfg.Messaging.MaxUnreadPerRecipient
	r.MessageRetention = time.Duration(cfg.Messaging.RetentionDays) * 24 * time.Hour
	return r
}

// Client skill rendering never changes default guidance. Explicit env false
// wins over connect/skill --messaging, matching the hook's disable override.
func messagingGuidanceEnabled(flags ...bool) bool {
	if v := os.Getenv("PUNK_MESSAGING"); v != "" {
		enabled, _ := strconv.ParseBool(v)
		return enabled
	}
	return len(flags) > 0 && flags[0]
}
