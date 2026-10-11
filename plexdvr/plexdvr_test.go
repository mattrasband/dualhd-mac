// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

package plexdvr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestEnumValues(t *testing.T) {
	got := enumValues("-1:Any|1791732600:09%3A30 AM|abc-def:6.1 KRMADT (PBS)")
	want := [][2]string{{"-1", "Any"}, {"1791732600", "09:30 AM"}, {"abc-def", "6.1 KRMADT (PBS)"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestChannelMatches(t *testing.T) {
	for _, c := range []struct {
		vcn, want string
		ok        bool
	}{
		{"9.1", "", true},
		{"9.1", "9.1", true},
		{"9.4", "9", true},
		{"9.1", "9.4", false},
		{"19.1", "9", false},
	} {
		if got := channelMatches(c.vcn, c.want); got != c.ok {
			t.Errorf("channelMatches(%q, %q) = %v", c.vcn, c.want, got)
		}
	}
}

// fakePlex serves a subscription template for an episode airing at two
// times on two channels, and records the subscription request it gets.
func fakePlex(t *testing.T, got *url.Values) *httptest.Server {
	t.Helper()
	tmpl := map[string]any{"MediaContainer": map[string]any{"SubscriptionTemplate": []any{map[string]any{
		"MediaSubscription": []any{
			map[string]any{
				"title": "This Episode", "type": 4, "targetLibrarySectionID": 1,
				"parameters": "hints%5Bguid%5D=plex%3A%2F%2Fepisode%2Fx",
				"Setting": []any{
					map[string]any{"id": "oneShot", "value": true},
					map[string]any{"id": "lineupChannel", "value": "", "enumValues": ":Any|ch-a:6.2 KRMADT2 (PBS Kids)|ch-b:6.1 KRMADT (PBS)"},
					map[string]any{"id": "startTimeslot", "value": -1, "enumValues": "-1:Any|1791732600:09%3A30 AM|1791993600:10%3A00 AM"},
				},
			},
			map[string]any{"title": "All Episodes", "type": 2, "targetLibrarySectionID": 1, "parameters": "x=1",
				"Setting": []any{map[string]any{"id": "onlyNewAirings", "value": 1}}},
		},
	}}}}
	sub := map[string]any{"MediaContainer": map[string]any{"MediaSubscription": []any{map[string]any{
		"key": "7", "type": 4, "title": "This Episode", "librarySectionTitle": "TV Shows",
		"Video": map[string]any{"title": "Ep", "grandparentTitle": "Show"},
		"MediaGrabOperation": []any{map[string]any{"status": "scheduled", "mediaIndex": 1, "Metadata": map[string]any{
			"title": "Ep",
			"Media": []any{
				map[string]any{"beginsAt": 1791732600, "endsAt": 1791734400, "channelVcn": "6.2"},
				map[string]any{"beginsAt": 1791993600, "endsAt": 1791995400, "channelVcn": "6.1"},
			},
		}}},
	}}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var v any
		switch {
		case r.Method == "GET" && r.URL.Path == "/media/subscriptions/template":
			v = tmpl
		case r.Method == "GET" && r.URL.Path == "/library/sections":
			v = map[string]any{"MediaContainer": map[string]any{"Directory": []any{
				map[string]any{"key": "1", "Location": []any{map[string]any{"id": 3}}}}}}
		case r.Method == "POST" && r.URL.Path == "/media/subscriptions":
			*got = r.URL.Query()
			v = sub
		default:
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(v)
	}))
}

func TestRecordPinnedAiring(t *testing.T) {
	var got url.Values
	srv := fakePlex(t, &got)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}

	// The Plex time choice is a time of day; a later airing at 10:00 matches.
	// (Fixed zone, so a daylight saving change can't shift it.)
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.UTC
	start := time.Unix(1791993600, 0).Add(6 * 24 * time.Hour)
	r, err := c.Record(context.Background(), "plex://episode/x", RecordOptions{Channel: "6.1", Start: start, PaddingAfter: 5})
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"hints[guid]":             "plex://episode/x",
		"type":                    "4",
		"targetLibrarySectionID":  "1",
		"targetSectionLocationID": "3",
		"prefs[oneShot]":          "true",
		"prefs[lineupChannel]":    "ch-b",
		"prefs[startTimeslot]":    "1791993600",
		"prefs[endOffsetMinutes]": "5",
	} {
		if got.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Get(k), want)
		}
	}
	if r.ID != "7" || r.Kind != "episode" || r.Show != "Show" {
		t.Errorf("recording = %+v", r)
	}
	// The grab's mediaIndex says which airing is recorded.
	if len(r.Upcoming) != 1 || r.Upcoming[0].Channel != "6.1" {
		t.Errorf("upcoming = %+v", r.Upcoming)
	}

	if _, err := c.Record(context.Background(), "plex://episode/x", RecordOptions{Channel: "99.9"}); err == nil {
		t.Error("unknown channel: no error")
	}
	if _, err := c.Record(context.Background(), "plex://episode/x", RecordOptions{Series: true, AllAirings: true}); err != nil {
		t.Fatal(err)
	}
	if got.Get("type") != "2" || got.Get("prefs[onlyNewAirings]") != "0" {
		t.Errorf("series: type=%s onlyNewAirings=%s", got.Get("type"), got.Get("prefs[onlyNewAirings]"))
	}
}
