package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
)

// runRetentionSweeps is the existing hourly server maintenance tick. Memory,
// message retention and member expiry are independent: disabling one must
// not disable the others. Never delete unacknowledged messages, and never
// delete a currently-listening member (region.ExpireMembers enforces both).
func runRetentionSweeps(ctx context.Context, log *slog.Logger, mem *memory.Store, reg *region.Store, memoryDays, memberExpiryDays int) {
	if memoryDays > 0 {
		n, err := mem.SweepRetention(ctx, time.Duration(memoryDays)*24*time.Hour)
		if err != nil {
			log.Error("retention sweep failed", "err", err)
		} else if n > 0 {
			log.Info("retention sweep", "rows", n)
		}
	}
	if memberExpiryDays > 0 {
		n, err := reg.ExpireMembers(ctx, time.Duration(memberExpiryDays)*24*time.Hour)
		if err != nil {
			log.Error("member expiry sweep failed", "err", err)
		} else if n > 0 {
			log.Info("member expiry sweep", "count", n)
		}
	}
	if reg.MessageRetention <= 0 {
		return
	}
	nss, err := reg.AllNamespaces(ctx)
	if err != nil {
		log.Error("message retention namespaces failed", "err", err)
		return
	}
	for _, ns := range nss {
		n, err := reg.SweepMessageRetention(ctx, ns)
		if err != nil {
			log.Error("message retention sweep failed", "ns", ns, "err", err)
		} else if n > 0 {
			log.Info("message retention sweep", "ns", ns, "rows", n)
		}
	}
}
