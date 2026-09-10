package rdsevent

import (
	"testing"

	. "github.com/onsi/gomega"
)

// RDS events have no unique ID, so eventKey is what stops the same event firing
// twice across overlapping poll windows. It must be deterministic and must
// distinguish events that differ in any identifying field.
func TestEventKey(t *testing.T) {
	g := NewWithT(t)

	base := eventState{
		SourceIdentifier: "my-db",
		SourceType:       "db-instance",
		Date:             "2026-07-21T09:00:00Z",
		Message:          "Multi-AZ failover completed",
	}

	// Deterministic: same event → same key.
	g.Expect(eventKey(base)).To(Equal(eventKey(base)))

	// Any identifying field change → different key.
	g.Expect(eventKey(base)).ToNot(Equal(eventKey(eventState{
		SourceIdentifier: "other-db", SourceType: base.SourceType, Date: base.Date, Message: base.Message,
	})))
	g.Expect(eventKey(base)).ToNot(Equal(eventKey(eventState{
		SourceIdentifier: base.SourceIdentifier, SourceType: base.SourceType, Date: "2026-07-21T10:00:00Z", Message: base.Message,
	})))
	g.Expect(eventKey(base)).ToNot(Equal(eventKey(eventState{
		SourceIdentifier: base.SourceIdentifier, SourceType: base.SourceType, Date: base.Date, Message: "Backup complete",
	})))

	// SourceArn is metadata, not identity — it doesn't change the key.
	withArn := base
	withArn.SourceArn = "arn:aws:rds:eu-west-2:123456789012:db:my-db"
	g.Expect(eventKey(withArn)).To(Equal(eventKey(base)))
}

func TestSplitCSV(t *testing.T) {
	g := NewWithT(t)
	g.Expect(splitCSV("failover, availability , ,backup")).To(Equal([]string{"failover", "availability", "backup"}))
	g.Expect(splitCSV("")).To(BeEmpty())
	g.Expect(splitCSV("   ")).To(BeEmpty())
}
