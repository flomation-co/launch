package slack

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestParseMentions_None(t *testing.T) {
	RegisterTestingT(t)

	m := ParseMentions("Can we remove the bastion hosts in AWS?")
	Expect(m).To(BeEmpty())
}

func TestParseMentions_SingleHandle(t *testing.T) {
	RegisterTestingT(t)

	// "@Tess Ashworth" arrives as the bot's user ID, not literal text.
	m := ParseMentions("<@U0TESS> what's the cost of these?")
	Expect(m).To(HaveLen(1))
	Expect(m["U0TESS"]).To(BeTrue())
	Expect(m["U0DAN"]).To(BeFalse())
}

func TestParseMentions_WithDisplayName(t *testing.T) {
	RegisterTestingT(t)

	// Slack sometimes encodes the token as <@ID|display-name>.
	m := ParseMentions("hey <@U0DAN|dan.marsh> can you look at this MR?")
	Expect(m).To(HaveLen(1))
	Expect(m["U0DAN"]).To(BeTrue())
}

func TestParseMentions_Multiple(t *testing.T) {
	RegisterTestingT(t)

	m := ParseMentions("<@U0DAN> and <@U0TESS> please coordinate")
	Expect(m).To(HaveLen(2))
	Expect(m["U0DAN"]).To(BeTrue())
	Expect(m["U0TESS"]).To(BeTrue())
}
