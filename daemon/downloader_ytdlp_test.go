package main

import "testing"

// The Turbo size-drift bug: the old progress template merged
// total_bytes,total_bytes_estimate into one field, and the download loop
// stored whatever came back as TotalBytes. For DASH downloads yt-dlp's
// total_bytes_estimate is a moving guess (average fragment size so far x
// fragment count) that starts far too high and shrinks — so the Size column
// drifted (e.g. ~5 GB at the start). These tests pin the split: the exact
// total and the estimate must arrive in separate fields, and a line with no
// exact total must yield total == 0 however large the estimate is.
func TestParseDownloadInfoLineExactTotal(t *testing.T) {
	line := `DOWNLOAD_INFO: 12.5%|1.20MiB/s|00:42|131072000|1073741824|NA|NA|NA|C:\Downloads\video.mp4`
	info, ok := parseDownloadInfoLine(line)
	if !ok {
		t.Fatalf("expected line to parse")
	}
	if info.total != 1073741824 {
		t.Errorf("total = %d, want 1073741824", info.total)
	}
	if info.estimate != 0 {
		t.Errorf("estimate = %d, want 0 (NA)", info.estimate)
	}
	if info.downloaded != 131072000 {
		t.Errorf("downloaded = %d, want 131072000", info.downloaded)
	}
	if info.filename != `C:\Downloads\video.mp4` {
		t.Errorf("filename = %q", info.filename)
	}
}

func TestParseDownloadInfoLineDashEstimateNotTotal(t *testing.T) {
	// DASH/Turbo line: no exact total (NA), estimate ~5 GB that will shrink
	// as fragments arrive. total must stay 0; the estimate must land in its
	// own field and never be mistaken for the file size.
	line := `DOWNLOAD_INFO:  3.1%|2.50MiB/s|01:10|52428800|NA|5368709120|2|412|C:\Downloads\video.f137.mp4`
	info, ok := parseDownloadInfoLine(line)
	if !ok {
		t.Fatalf("expected line to parse")
	}
	if info.total != 0 {
		t.Errorf("total = %d, want 0 (no exact total for DASH)", info.total)
	}
	if info.estimate != 5368709120 {
		t.Errorf("estimate = %d, want 5368709120", info.estimate)
	}
	if info.fragIndex != 2 || info.fragCount != 412 {
		t.Errorf("fragments = %d/%d, want 2/412", info.fragIndex, info.fragCount)
	}
}

func TestParseDownloadInfoLineRejectsShortLines(t *testing.T) {
	for _, line := range []string{
		"",
		"DOWNLOAD_INFO:",
		"DOWNLOAD_INFO: 12.5%|1.20MiB/s",
		"[download]  12.5% of 1.00GiB at 1.20MiB/s ETA 00:42",
	} {
		if _, ok := parseDownloadInfoLine(line); ok {
			t.Errorf("line %q should not parse", line)
		}
	}
}
