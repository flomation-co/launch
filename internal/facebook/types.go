package facebook

// MessengerMessage represents a parsed inbound Messenger message.
type MessengerMessage struct {
	SenderPSID    string       `json:"sender_id"`
	RecipientID   string       `json:"recipient_id"` // Page ID
	Text          string       `json:"message_text"`
	MessageID     string       `json:"message_id"`
	Timestamp     int64        `json:"timestamp"`
	Attachments   []Attachment `json:"attachments,omitempty"`
	IsPostback    bool         `json:"is_postback"`
	PostbackTitle string       `json:"postback_title,omitempty"`
}

// Attachment represents a media attachment in a Messenger message.
type Attachment struct {
	Type       string `json:"type"` // image, audio, video, file, location
	PayloadURL string `json:"payload_url,omitempty"`
}

// FeedEvent represents a parsed Facebook Page feed change event.
type FeedEvent struct {
	PageID       string `json:"page_id"`
	EventType    string `json:"event_type"` // comment, reaction, post, share
	Verb         string `json:"verb"`       // add, edit, remove
	ItemID       string `json:"item_id"`    // comment_id or post_id
	ParentID     string `json:"parent_id"`  // post_id for comments
	PostID       string `json:"post_id"`
	SenderID     string `json:"sender_id"`
	SenderName   string `json:"sender_name"`
	Message      string `json:"message"`       // comment text or post content
	ReactionType string `json:"reaction_type"` // like, love, haha, wow, sad, angry
	CreatedTime  int64  `json:"created_time"`
}
