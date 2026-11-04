package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteNetscapeCookiesFile(t *testing.T) {
	tmpDir := t.TempDir()
	cookieFile := filepath.Join(tmpDir, "cookies.txt")

	cookies := []Cookie{
		{
			Name:           "SESSION_ID",
			Value:          "secret123",
			Domain:         ".youtube.com",
			Path:           "/",
			Secure:         true,
			ExpirationDate: 1780000000,
		},
		{
			Name:     "pref",
			Value:    "dark_mode",
			Domain:   "example.com",
			HostOnly: true,
			Path:     "/home",
			Secure:   false,
		},
	}

	err := WriteNetscapeCookiesFile(cookies, cookieFile)
	if err != nil {
		t.Fatalf("WriteNetscapeCookiesFile returned error: %v", err)
	}

	data, err := os.ReadFile(cookieFile)
	if err != nil {
		t.Fatalf("Failed to read created cookie file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "# Netscape HTTP Cookie File") {
		t.Errorf("Expected Netscape header, got:\n%s", content)
	}

	// Verify first cookie
	if !strings.Contains(content, ".youtube.com\tTRUE\t/\tTRUE\t1780000000\tSESSION_ID\tsecret123") {
		t.Errorf("Cookie line 1 formatted incorrectly:\n%s", content)
	}

	// Verify second cookie (HostOnly is true, so includeSubdomains should be FALSE)
	if !strings.Contains(content, "example.com\tFALSE\t/home\tFALSE\t") {
		t.Errorf("Cookie line 2 formatted incorrectly:\n%s", content)
	}
}

func TestFormatCookieHeader(t *testing.T) {
	cookies := []Cookie{
		{Name: "session", Value: "abc"},
		{Name: "theme", Value: "dark"},
	}

	header := FormatCookieHeader(cookies)
	expected := "session=abc; theme=dark"
	if header != expected {
		t.Errorf("FormatCookieHeader = %q, expected %q", header, expected)
	}

	if empty := FormatCookieHeader(nil); empty != "" {
		t.Errorf("Expected empty string for nil cookies, got %q", empty)
	}
}
