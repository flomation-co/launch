package twilio

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// TwiML response types for generating XML responses to Twilio webhooks.

// Response is the root TwiML element.
type Response struct {
	XMLName xml.Name      `xml:"Response"`
	Verbs   []interface{} `xml:",any"`
}

// Say instructs Twilio to read text to the caller.
type Say struct {
	XMLName  xml.Name `xml:"Say"`
	Voice    string   `xml:"voice,attr,omitempty"`
	Language string   `xml:"language,attr,omitempty"`
	Text     string   `xml:",chardata"`
}

// Play instructs Twilio to play an audio file.
type Play struct {
	XMLName xml.Name `xml:"Play"`
	URL     string   `xml:",chardata"`
}

// Hangup ends the call.
type Hangup struct {
	XMLName xml.Name `xml:"Hangup"`
}

// Connect connects the call to a stream or other endpoint.
type Connect struct {
	XMLName xml.Name `xml:"Connect"`
	Stream  *Stream  `xml:",omitempty"`
}

// Stream starts a Media Stream WebSocket connection.
type Stream struct {
	XMLName    xml.Name    `xml:"Stream"`
	URL        string      `xml:"url,attr"`
	Parameters []Parameter `xml:",omitempty"`
}

// Parameter passes custom data to the stream.
type Parameter struct {
	XMLName xml.Name `xml:"Parameter"`
	Name    string   `xml:"name,attr"`
	Value   string   `xml:"value,attr"`
}

// EmptyResponse returns a minimal TwiML response (no action).
func EmptyResponse() string {
	return `<?xml version="1.0" encoding="UTF-8"?><Response/>`
}

// MediaStreamResponse generates TwiML that starts a bidirectional media stream.
// Uses string building to avoid xml.Marshal issues with nested verb elements.
func MediaStreamResponse(wsURL string, params map[string]string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<Response><Connect><Stream url="`)
	sb.WriteString(escapeXMLAttr(wsURL))
	sb.WriteString(`">`)
	for k, v := range params {
		fmt.Fprintf(&sb, `<Parameter name="%s" value="%s"/>`, escapeXMLAttr(k), escapeXMLAttr(v))
	}
	sb.WriteString(`</Stream></Connect></Response>`)
	return sb.String()
}

// SayResponse generates a simple TwiML Say response.
func SayResponse(text, voice string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<Response><Say`)
	if voice != "" {
		fmt.Fprintf(&sb, ` voice="%s"`, escapeXMLAttr(voice))
	}
	sb.WriteString(`>`)
	sb.WriteString(escapeXMLText(text))
	sb.WriteString(`</Say></Response>`)
	return sb.String()
}

func escapeXMLAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func escapeXMLText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
