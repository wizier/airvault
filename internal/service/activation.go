package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wizier/airvault/internal/engine"
)

// Session-mode activation endpoints and headers (canon: libideviceactivation).
// The urlencoded deviceActivation POST follows pymobiledevice3; canon C sends
// the same fields as multipart.
const (
	activationDRMHandshakeURL = "https://albert.apple.com/deviceservices/drmHandshake"
	activationURL             = "https://albert.apple.com/deviceservices/deviceActivation"
	activationUserAgent       = "iOS Device Activator (MobileActivation-592.103.2)"
)

const (
	errorCodeActivationLock   = "activation_lock"
	errorCodeActivationFailed = "activation_failed"
)

var activationHTTPClient = &http.Client{Timeout: 45 * time.Second}

// activateIfNeeded activates a Setup-Assistant phone before a restore: session
// blob → Apple drmHandshake → activation info → deviceActivation → record. An
// activated phone is a no-op; an Apple ID form reply means Activation Lock.
func (s *Service) activateIfNeeded(run *runReservation, deviceName string) (string, error) {
	ctx, udid := run.ctx, engine.DeviceID(run.udid)
	state, err := s.engine.ActivationState(ctx, udid)
	if err != nil {
		// The phone stays the authority; the restore itself surfaces real errors.
		slog.DebugContext(ctx, "restore: activation preflight unavailable", "device", deviceName, "error", err)
		return "", nil
	}
	if state != "Unactivated" {
		return "", nil
	}
	slog.InfoContext(ctx, "restore: phone is unactivated, activating with Apple", "device", deviceName, "udid", run.udid)
	s.setRunStage(run, StageActivating)
	defer s.setRunStage(run, StageRestoring)

	blob, err := s.engine.ActivationSessionInfo(ctx, udid)
	if err != nil {
		return errorCodeActivationFailed, fmt.Errorf("activation: session info: %w", err)
	}
	handshake, _, status, err := activationPost(ctx, activationDRMHandshakeURL,
		"application/x-apple-plist", bytes.NewReader(blob))
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("activation server replied with HTTP %d", status)
	}
	if err != nil {
		return errorCodeActivationFailed, fmt.Errorf("activation: drmHandshake: %w", err)
	}
	info, err := s.engine.ActivationInfo(ctx, udid, handshake)
	if err != nil {
		return errorCodeActivationFailed, fmt.Errorf("activation: activation info: %w", err)
	}
	form := url.Values{"activation-info": []string{string(info)}}
	record, header, status, err := activationPost(ctx, activationURL,
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return errorCodeActivationFailed, fmt.Errorf("activation: deviceActivation: %w", err)
	}
	applyRecord, code, err := classifyActivationReply(header.Get("Content-Type"), record)
	switch {
	case err != nil:
		return code, err
	case !applyRecord:
		slog.InfoContext(ctx, "restore: Apple reports the phone as already activated", "device", deviceName, "udid", run.udid)
		return "", nil
	case status != http.StatusOK:
		// A record only counts on 200; otherwise it is an error page that parsed.
		return errorCodeActivationFailed, fmt.Errorf("activation: Apple replied %d", status)
	}
	headers := make(map[string]string, len(header))
	for key := range header {
		headers[key] = header.Get(key)
	}
	if err := s.engine.ActivationFinish(ctx, udid, record, headers); err != nil {
		return errorCodeActivationFailed, fmt.Errorf("activation: applying the record: %w", err)
	}
	slog.InfoContext(ctx, "restore: phone activated", "device", deviceName, "udid", run.udid)
	return "", nil
}

// classifyActivationReply reads Apple's deviceActivation answer: a record to
// apply, an "already activated" ack (applyRecord=false, nil error), or a
// refusal. BuddyML shapes mirror libideviceactivation's parser.
func classifyActivationReply(contentType string, body []byte) (applyRecord bool, code string, err error) {
	if strings.Contains(contentType, "x-buddyml") {
		page := parseBuddyML(body)
		switch {
		case page.errorTitle != "":
			return false, errorCodeActivationFailed, fmt.Errorf("activation: Apple refused: %s", page.errorTitle)
		case page.acknowledged:
			return false, "", nil
		case page.credentialForm:
			return false, errorCodeActivationLock, fmt.Errorf(
				"activation: Apple requires the owner's Apple ID for this phone (Activation Lock)")
		default:
			return false, errorCodeActivationFailed, fmt.Errorf("activation: Apple returned an unrecognized activation page")
		}
	}
	switch {
	case len(body) == 0:
		return false, errorCodeActivationFailed, fmt.Errorf("activation: Apple returned an empty activation record")
	case !strings.Contains(contentType, "xml"):
		return false, errorCodeActivationFailed, fmt.Errorf(
			"activation: unexpected activation reply (%s)", contentType)
	}
	return true, "", nil
}

// buddymlPage is the minimal read of an x-buddyml reply: a top-level error
// title, the "already activated" ack, or a credential form (Activation Lock).
type buddymlPage struct {
	errorTitle     string
	acknowledged   bool
	credentialForm bool
}

// parseBuddyML walks the XML once; a top-level navigationBar only appears on
// error pages (forms carry theirs inside <page>), matching the canon parser.
func parseBuddyML(body []byte) buddymlPage {
	var page buddymlPage
	decoder := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return page
		}
		switch element := token.(type) {
		case xml.StartElement:
			if depth == 1 {
				switch element.Name.Local {
				case "navigationBar", "alert":
					if page.errorTitle == "" {
						page.errorTitle = xmlAttr(element, "title")
					}
				case "clientInfo":
					if xmlAttr(element, "ack-received") == "true" {
						page.acknowledged = true
					}
				}
			}
			if element.Name.Local == "editableTextRow" {
				page.credentialForm = true
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
}

func xmlAttr(element xml.StartElement, name string) string {
	for _, attr := range element.Attr {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

// activationPost returns Apple's reply verbatim and reports the status rather
// than enforcing it: a refusal page carries the reason in its body, and both
// canons classify that body whatever the status was.
func activationPost(ctx context.Context, endpoint, contentType string, body io.Reader) ([]byte, http.Header, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, nil, 0, err
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/xml")
	request.Header.Set("User-Agent", activationUserAgent)
	response, err := activationHTTPClient.Do(request)
	if err != nil {
		return nil, nil, 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, nil, response.StatusCode, err
	}
	return payload, response.Header, response.StatusCode, nil
}
