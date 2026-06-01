package twilio

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// VerifyResult contains the results of verifying Twilio credentials
// and phone number configuration.
type VerifyResult struct {
	OK                bool               `json:"ok"`
	Error             string             `json:"error,omitempty"`
	AccountName       string             `json:"account_name,omitempty"`
	AccountStatus     string             `json:"account_status,omitempty"`
	PhoneFound        bool               `json:"phone_found"`
	PhoneCapabilities *PhoneCapabilities `json:"phone_capabilities,omitempty"`
	VoiceURL          string             `json:"voice_url,omitempty"`
	SMSURL            string             `json:"sms_url,omitempty"`
	Checks            []VerifyCheck      `json:"checks"`
}

// PhoneCapabilities describes what a Twilio phone number can do.
type PhoneCapabilities struct {
	Voice bool `json:"voice"`
	SMS   bool `json:"sms"`
	MMS   bool `json:"mms"`
}

// VerifyCheck represents a single verification check result.
type VerifyCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// VerifyCredentials checks Twilio Account SID, Auth Token, and phone number
// configuration against the Twilio REST API.
func VerifyCredentials(accountSID, authToken, phoneNumber, expectedVoiceURL, expectedSMSURL string) *VerifyResult {
	result := &VerifyResult{
		OK:     true,
		Checks: []VerifyCheck{},
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// Check 1: Verify credentials by fetching account info
	accountURL := fmt.Sprintf("%s/Accounts/%s.json", twilioAPIBase, accountSID)
	req, _ := http.NewRequest(http.MethodGet, accountURL, nil)
	req.SetBasicAuth(accountSID, authToken)

	resp, err := client.Do(req)
	if err != nil {
		result.OK = false
		result.Error = fmt.Sprintf("Failed to connect to Twilio API: %v", err)
		result.Checks = append(result.Checks, VerifyCheck{
			Name:   "Credentials",
			Passed: false,
			Detail: "Unable to reach Twilio API",
		})
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode == 401 {
		result.OK = false
		result.Error = "Invalid Account SID or Auth Token"
		result.Checks = append(result.Checks, VerifyCheck{
			Name:   "Credentials",
			Passed: false,
			Detail: "Authentication failed — check Account SID and Auth Token",
		})
		return result
	}

	if resp.StatusCode != 200 {
		result.OK = false
		result.Error = fmt.Sprintf("Twilio API returned %d", resp.StatusCode)
		result.Checks = append(result.Checks, VerifyCheck{
			Name:   "Credentials",
			Passed: false,
			Detail: fmt.Sprintf("Unexpected status %d from Twilio", resp.StatusCode),
		})
		return result
	}

	var account struct {
		FriendlyName string `json:"friendly_name"`
		Status       string `json:"status"`
	}
	_ = json.Unmarshal(body, &account)

	result.AccountName = account.FriendlyName
	result.AccountStatus = account.Status

	credPassed := account.Status == "active"
	result.Checks = append(result.Checks, VerifyCheck{
		Name:   "Credentials",
		Passed: credPassed,
		Detail: fmt.Sprintf("Account: %s (%s)", account.FriendlyName, account.Status),
	})
	if !credPassed {
		result.OK = false
	}

	// Check 2: Find the phone number
	if phoneNumber == "" {
		result.Checks = append(result.Checks, VerifyCheck{
			Name:   "Phone Number",
			Passed: false,
			Detail: "No phone number configured",
		})
		result.OK = false
		return result
	}

	phoneURL := fmt.Sprintf("%s/Accounts/%s/IncomingPhoneNumbers.json?PhoneNumber=%s",
		twilioAPIBase, accountSID, url.QueryEscape(phoneNumber))
	req2, _ := http.NewRequest(http.MethodGet, phoneURL, nil)
	req2.SetBasicAuth(accountSID, authToken)

	resp2, err := client.Do(req2)
	if err != nil {
		result.Checks = append(result.Checks, VerifyCheck{
			Name:   "Phone Number",
			Passed: false,
			Detail: fmt.Sprintf("Failed to look up phone number: %v", err),
		})
		result.OK = false
		return result
	}
	defer func() { _ = resp2.Body.Close() }()

	body2, _ := io.ReadAll(io.LimitReader(resp2.Body, 8192))

	var phoneResult struct {
		IncomingPhoneNumbers []struct {
			PhoneNumber  string `json:"phone_number"`
			FriendlyName string `json:"friendly_name"`
			Capabilities struct {
				Voice bool `json:"voice"`
				SMS   bool `json:"sms"`
				MMS   bool `json:"mms"`
			} `json:"capabilities"`
			VoiceURL string `json:"voice_url"`
			SMSURL   string `json:"sms_url"`
		} `json:"incoming_phone_numbers"`
	}
	_ = json.Unmarshal(body2, &phoneResult)

	if len(phoneResult.IncomingPhoneNumbers) == 0 {
		result.PhoneFound = false
		result.Checks = append(result.Checks, VerifyCheck{
			Name:   "Phone Number",
			Passed: false,
			Detail: fmt.Sprintf("Phone number %s not found in this account", phoneNumber),
		})
		result.OK = false
		return result
	}

	phone := phoneResult.IncomingPhoneNumbers[0]
	result.PhoneFound = true
	result.PhoneCapabilities = &PhoneCapabilities{
		Voice: phone.Capabilities.Voice,
		SMS:   phone.Capabilities.SMS,
		MMS:   phone.Capabilities.MMS,
	}
	result.VoiceURL = phone.VoiceURL
	result.SMSURL = phone.SMSURL

	result.Checks = append(result.Checks, VerifyCheck{
		Name:   "Phone Number",
		Passed: true,
		Detail: fmt.Sprintf("Found: %s (%s)", phone.PhoneNumber, phone.FriendlyName),
	})

	// Check 3: Voice capability
	result.Checks = append(result.Checks, VerifyCheck{
		Name:   "Voice Capability",
		Passed: phone.Capabilities.Voice,
		Detail: boolToStatus(phone.Capabilities.Voice, "Voice calls enabled", "Voice calls not available on this number"),
	})
	if !phone.Capabilities.Voice {
		result.OK = false
	}

	// Check 4: SMS capability
	result.Checks = append(result.Checks, VerifyCheck{
		Name:   "SMS Capability",
		Passed: phone.Capabilities.SMS,
		Detail: boolToStatus(phone.Capabilities.SMS, "SMS enabled", "SMS not available on this number"),
	})

	// Check 5: Voice webhook URL
	if expectedVoiceURL != "" {
		voiceMatch := phone.VoiceURL == expectedVoiceURL
		detail := "Not configured"
		if phone.VoiceURL != "" {
			if voiceMatch {
				detail = "Correctly configured"
			} else {
				detail = fmt.Sprintf("Set to %s (expected %s)", phone.VoiceURL, expectedVoiceURL)
			}
		}
		result.Checks = append(result.Checks, VerifyCheck{
			Name:   "Voice Webhook",
			Passed: voiceMatch,
			Detail: detail,
		})
	}

	// Check 6: SMS webhook URL
	if expectedSMSURL != "" {
		smsMatch := phone.SMSURL == expectedSMSURL
		detail := "Not configured"
		if phone.SMSURL != "" {
			if smsMatch {
				detail = "Correctly configured"
			} else {
				detail = fmt.Sprintf("Set to %s (expected %s)", phone.SMSURL, expectedSMSURL)
			}
		}
		result.Checks = append(result.Checks, VerifyCheck{
			Name:   "SMS Webhook",
			Passed: smsMatch,
			Detail: detail,
		})
	}

	return result
}

func boolToStatus(val bool, trueMsg, falseMsg string) string {
	if val {
		return trueMsg
	}
	return falseMsg
}
