package activation

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Endpoints and headers follow libideviceactivation. The urlencoded
// deviceActivation POST follows pymobiledevice3; libideviceactivation sends the
// same fields as multipart.
const (
	drmHandshakeURL = "https://albert.apple.com/deviceservices/drmHandshake"
	activationURL   = "https://albert.apple.com/deviceservices/deviceActivation"
	userAgent       = "iOS Device Activator (MobileActivation-592.103.2)"
)

var ErrActivationLock = errors.New("activation: Apple requires the owner's Apple ID for this phone (Activation Lock)")

var httpClient = &http.Client{Timeout: 45 * time.Second}

func Handshake(ctx context.Context, sessionInfo []byte) ([]byte, error) {
	handshake, _, status, err := post(ctx, drmHandshakeURL, "application/x-apple-plist", bytes.NewReader(sessionInfo))
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("activation server replied with HTTP %d", status)
	}
	if err != nil {
		return nil, fmt.Errorf("activation: drmHandshake: %w", err)
	}
	return handshake, nil
}

// A nil record without an error means Apple already considers the phone
// activated.
func RequestRecord(ctx context.Context, info []byte) ([]byte, map[string]string, error) {
	form := url.Values{"activation-info": []string{string(info)}}
	record, header, status, err := post(ctx, activationURL,
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("activation: deviceActivation: %w", err)
	}
	applyRecord, err := classifyReply(header.Get("Content-Type"), record)
	switch {
	case err != nil:
		return nil, nil, err
	case !applyRecord:
		return nil, nil, nil
	case status != http.StatusOK:
		// A record only counts on 200; otherwise it is an error page that parsed.
		return nil, nil, fmt.Errorf("activation: Apple replied %d", status)
	}
	headers := make(map[string]string, len(header))
	for key := range header {
		headers[key] = header.Get(key)
	}
	return record, headers, nil
}

// An "already activated" ack is applyRecord=false with a nil error. BuddyML
// shapes mirror libideviceactivation's parser.
func classifyReply(contentType string, body []byte) (applyRecord bool, err error) {
	if strings.Contains(contentType, "x-buddyml") {
		page := parseBuddyML(body)
		switch {
		case page.errorTitle != "":
			return false, fmt.Errorf("activation: Apple refused: %s", page.errorTitle)
		case page.acknowledged:
			return false, nil
		case page.credentialForm:
			return false, ErrActivationLock
		default:
			return false, fmt.Errorf("activation: Apple returned an unrecognized activation page")
		}
	}
	switch {
	case len(body) == 0:
		return false, fmt.Errorf("activation: Apple returned an empty activation record")
	case !strings.Contains(contentType, "xml"):
		return false, fmt.Errorf("activation: unexpected activation reply (%s)", contentType)
	}
	return true, nil
}

type buddymlPage struct {
	errorTitle     string
	acknowledged   bool
	credentialForm bool
}

// A top-level navigationBar only appears on error pages (forms carry theirs
// inside <page>), matching libideviceactivation's parser.
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

// post reports the status rather than enforcing it: a refusal page carries the
// reason in its body, which is classified whatever the status was.
func post(ctx context.Context, endpoint, contentType string, body io.Reader) ([]byte, http.Header, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, nil, 0, err
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/xml")
	request.Header.Set("User-Agent", userAgent)
	response, err := httpClient.Do(request)
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
