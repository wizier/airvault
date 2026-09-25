package activation

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyReply(t *testing.T) {
	record := []byte(`<?xml version="1.0"?><plist><dict/></plist>`)
	appleIDForm := []byte(`<xmlui><page><navigationBar title="Activate iPhone"/>` +
		`<tableView><section><editableTextRow id="login" label="Apple ID"/>` +
		`<editableTextRow id="password" label="Password" secure="true"/></section></tableView></page></xmlui>`)
	serverError := []byte(`<xmlui><navigationBar title="Activation Error"/></xmlui>`)
	alreadyActivated := []byte(`<xmlui><clientInfo ack-received="true"/></xmlui>`)
	cases := []struct {
		name        string
		contentType string
		body        []byte
		wantApply   bool
		wantLock    bool
		wantErr     string
	}{
		{"session record", "text/xml", record, true, false, ""},
		{"record with charset", "application/xml; charset=UTF-8", record, true, false, ""},
		{"apple id form is activation lock", "application/x-buddyml", appleIDForm, false, true, "Activation Lock"},
		{"error page keeps apple's title", "application/x-buddyml", serverError, false, false, "Activation Error"},
		{"ack means already activated", "application/x-buddyml", alreadyActivated, false, false, ""},
		{"unrecognized page is not lock", "application/x-buddyml", []byte("<xmlui/>"), false, false, "unrecognized"},
		{"empty reply", "text/xml", nil, false, false, "empty"},
		{"html error page", "text/html", []byte("<html/>"), false, false, "unexpected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyRecord, err := classifyReply(tc.contentType, tc.body)
			if applyRecord != tc.wantApply || errors.Is(err, ErrActivationLock) != tc.wantLock || (err != nil) != (tc.wantErr != "") {
				t.Fatalf("classifyReply(%q) = %v, %v; want %v, lock=%v, err=%v",
					tc.contentType, applyRecord, err, tc.wantApply, tc.wantLock, tc.wantErr != "")
			}
			if err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}
