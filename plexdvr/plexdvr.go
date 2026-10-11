// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Package plexdvr searches a Plex Media Server's Live TV guide and schedules
// DVR recordings, using the same HTTP API as Plex's own apps.
//
// The recording ("subscription") API isn't publicly documented. Record follows
// what the apps do: fetch the subscription template for a guide item, which
// lists the recording choices and their settings, then POST the chosen one.
package plexdvr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	BaseURL string // e.g. http://127.0.0.1:32400
	Token   string
	HTTP    *http.Client

	mu       sync.Mutex
	provider string // guide provider path, e.g. /tv.plex.providers.epg.cloud:2
}

// NewLocal returns a client for the Plex server on this Mac. PLEX_URL and
// PLEX_TOKEN override the defaults; otherwise the token is read from Plex
// Media Server's preferences.
func NewLocal() (*Client, error) {
	base := os.Getenv("PLEX_URL")
	if base == "" {
		base = "http://127.0.0.1:32400"
	}
	token := os.Getenv("PLEX_TOKEN")
	if token == "" {
		out, err := exec.Command("defaults", "read", "com.plexapp.plexmediaserver", "PlexOnlineToken").Output()
		if err != nil {
			return nil, fmt.Errorf("no PLEX_TOKEN set, and couldn't read it from Plex Media Server's preferences: %w", err)
		}
		token = strings.TrimSpace(string(out))
	}
	return &Client{BaseURL: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Plex-Token", c.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("plex: %s %s: %s %s", method, strings.SplitN(path, "?", 2)[0], resp.Status, msg)
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

// Plex API shapes (only the fields used here).

type mediaContainer struct {
	MediaContainer struct {
		MediaProvider        []mediaProvider        `json:"MediaProvider"`
		Hub                  []hub                  `json:"Hub"`
		Metadata             []metadata             `json:"Metadata"`
		SubscriptionTemplate []subscriptionTemplate `json:"SubscriptionTemplate"`
		MediaSubscription    []mediaSubscription    `json:"MediaSubscription"`
		Directory            []librarySection       `json:"Directory"`
	} `json:"MediaContainer"`
}

type mediaProvider struct {
	Identifier string `json:"identifier"`
	Protocols  string `json:"protocols"`
	Feature    []struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	} `json:"Feature"`
}

type hub struct {
	Type     string     `json:"type"`
	Metadata []metadata `json:"Metadata"`
}

type metadata struct {
	GUID             string  `json:"guid"`
	Type             string  `json:"type"`
	Title            string  `json:"title"`
	GrandparentTitle string  `json:"grandparentTitle"`
	Year             flexInt `json:"year"`
	Index            flexInt `json:"index"`
	ParentIndex      flexInt `json:"parentIndex"`
	Summary          string  `json:"summary"`
	Media            []media `json:"Media"`
}

type media struct {
	BeginsAt     int64  `json:"beginsAt"`
	EndsAt       int64  `json:"endsAt"`
	ChannelVcn   string `json:"channelVcn"`
	ChannelTitle string `json:"channelTitle"`
	Premiere     any    `json:"premiere"`
}

type subscriptionTemplate struct {
	MediaSubscription []templateChoice `json:"MediaSubscription"`
}

type templateChoice struct {
	Title                  string    `json:"title"`
	Type                   int       `json:"type"`
	TargetLibrarySectionID int       `json:"targetLibrarySectionID"`
	Parameters             string    `json:"parameters"`
	Setting                []setting `json:"Setting"`
}

type setting struct {
	ID         string `json:"id"`
	Value      any    `json:"value"`
	EnumValues string `json:"enumValues"`
}

type mediaSubscription struct {
	Key                 string    `json:"key"`
	Type                int       `json:"type"`
	Title               string    `json:"title"`
	LibrarySectionTitle string    `json:"librarySectionTitle"`
	Video               *metadata `json:"Video"`
	Directory           *metadata `json:"Directory"`
	Setting             []setting `json:"Setting"`
	MediaGrabOperation  []struct {
		Status     string   `json:"status"`
		MediaIndex int      `json:"mediaIndex"` // which of Metadata.Media is recorded
		Metadata   metadata `json:"Metadata"`
	} `json:"MediaGrabOperation"`
}

type librarySection struct {
	Key      string `json:"key"`
	Location []struct {
		ID int `json:"id"`
	} `json:"Location"`
}

// flexInt accepts numbers sent either as JSON numbers or strings.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	*f = flexInt(n)
	return nil
}

func settingValue(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// enumValues parses "value:label|value:label" (labels are URL-encoded).
func enumValues(s string) [][2]string {
	var out [][2]string
	for _, e := range strings.Split(s, "|") {
		v, label, ok := strings.Cut(e, ":")
		if !ok {
			continue
		}
		if l, err := url.QueryUnescape(label); err == nil {
			label = l
		}
		out = append(out, [2]string{v, label})
	}
	return out
}

// Public types.

// Airing is one scheduled broadcast.
type Airing struct {
	Channel     string    `json:"channel"`      // virtual channel, e.g. "9.1"
	ChannelName string    `json:"channel_name"` // e.g. "9.1 KUSADT (NBC)"
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	New         bool      `json:"new,omitempty"` // Plex marks it as a premiere
}

// Program is something in the guide: a show, an episode (sports events are
// episodes too) or a movie.
type Program struct {
	GUID    string   `json:"guid"`
	Type    string   `json:"type"` // show, episode or movie
	Title   string   `json:"title"`
	Show    string   `json:"show,omitempty"`
	Season  int      `json:"season,omitempty"`
	Episode int      `json:"episode,omitempty"`
	Year    int      `json:"year,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Airings []Airing `json:"airings,omitempty"`
}

func (m metadata) program(after time.Time) Program {
	p := Program{
		GUID: m.GUID, Type: m.Type, Title: m.Title, Year: int(m.Year), Summary: m.Summary,
	}
	if m.Type == "episode" {
		p.Show, p.Season, p.Episode = m.GrandparentTitle, int(m.ParentIndex), int(m.Index)
	}
	for _, md := range m.Media {
		a := md.airing()
		if !a.End.After(after) {
			continue
		}
		p.Airings = append(p.Airings, a)
	}
	sort.Slice(p.Airings, func(i, j int) bool { return p.Airings[i].Start.Before(p.Airings[j].Start) })
	return p
}

func (md media) airing() Airing {
	return Airing{
		Channel:     md.ChannelVcn,
		ChannelName: md.ChannelTitle,
		Start:       time.Unix(md.BeginsAt, 0),
		End:         time.Unix(md.EndsAt, 0),
		New:         settingValue(md.Premiere) == "1",
	}
}

// Provider returns the guide provider's path on the server.
func (c *Client) Provider(ctx context.Context) (string, error) {
	c.mu.Lock()
	p := c.provider
	c.mu.Unlock()
	if p != "" {
		return p, nil
	}
	var mc mediaContainer
	if err := c.do(ctx, "GET", "/media/providers", &mc); err != nil {
		return "", err
	}
	for _, mp := range mc.MediaContainer.MediaProvider {
		if !strings.Contains(mp.Protocols, "livetv") {
			continue
		}
		for _, f := range mp.Feature {
			if f.Type == "grid" {
				p = strings.TrimSuffix(f.Key, "/grid")
			}
		}
	}
	if p == "" {
		return "", errors.New("plex has no Live TV guide; set up Live TV & DVR first")
	}
	c.mu.Lock()
	c.provider = p
	c.mu.Unlock()
	return p, nil
}

// Search finds shows, episodes and movies in the guide by title. Episodes and
// movies are returned only if they air (or are on air) after now.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Program, error) {
	prov, err := c.Provider(ctx)
	if err != nil {
		return nil, err
	}
	var mc mediaContainer
	q := url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}}
	if err := c.do(ctx, "GET", prov+"/hubs/search?"+q.Encode(), &mc); err != nil {
		return nil, err
	}
	now := time.Now()
	var out []Program
	for _, h := range mc.MediaContainer.Hub {
		if h.Type != "show" && h.Type != "episode" && h.Type != "movie" {
			continue
		}
		for _, m := range h.Metadata {
			p := m.program(now)
			if p.Type != "show" && len(p.Airings) == 0 {
				continue
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// GuideEntry is one airing in the guide grid.
type GuideEntry struct {
	Program
	Airing
}

// Guide lists what airs between from and to, optionally only on channels
// matching channel ("9.1", or "9" for all of 9.x).
func (c *Client) Guide(ctx context.Context, from, to time.Time, channel string) ([]GuideEntry, error) {
	prov, err := c.Provider(ctx)
	if err != nil {
		return nil, err
	}
	var out []GuideEntry
	for _, typ := range []string{"4", "1"} { // episodes, movies
		q := fmt.Sprintf("type=%s&beginsAt%%3C=%d&endsAt%%3E=%d", typ, to.Unix(), from.Unix())
		var mc mediaContainer
		if err := c.do(ctx, "GET", prov+"/grid?"+q, &mc); err != nil {
			return nil, err
		}
		for _, m := range mc.MediaContainer.Metadata {
			p := m.program(time.Time{})
			airings := p.Airings
			p.Airings = nil
			p.Summary = ""
			for _, a := range airings {
				if !a.Start.Before(to) || !a.End.After(from) || !channelMatches(a.Channel, channel) {
					continue
				}
				out = append(out, GuideEntry{p, a})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Channel != out[j].Channel {
			return channelLess(out[i].Channel, out[j].Channel)
		}
		return out[i].Start.Before(out[j].Start)
	})
	return out, nil
}

func channelMatches(vcn, want string) bool {
	if want == "" || vcn == want {
		return true
	}
	return !strings.Contains(want, ".") && strings.HasPrefix(vcn, want+".")
}

func channelLess(a, b string) bool {
	am, an, _ := strings.Cut(a, ".")
	bm, bn, _ := strings.Cut(b, ".")
	ai, _ := strconv.Atoi(am)
	bi, _ := strconv.Atoi(bm)
	if ai != bi {
		return ai < bi
	}
	ani, _ := strconv.Atoi(an)
	bni, _ := strconv.Atoi(bn)
	return ani < bni
}

// RecordOptions choose what Record schedules.
type RecordOptions struct {
	// Series records every episode of the show, instead of one episode or movie.
	Series bool
	// AllAirings (series only) records repeats too, not just new episodes.
	AllAirings bool
	// Channel limits recording to one virtual channel, e.g. "9.1".
	Channel string
	// Start limits recording to airings at this time of day (Plex's
	// "start time" choice). With Channel, it picks a specific airing of a
	// one-off that airs more than once; the first such airing is recorded.
	Start time.Time
	// Minutes to start early and end late.
	PaddingBefore, PaddingAfter int
}

// Recording is a scheduled recording rule and its upcoming airings.
type Recording struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"` // episode (one-off, including sports events), series or movie
	Title    string     `json:"title"`
	Show     string     `json:"show,omitempty"`
	Library  string     `json:"library,omitempty"`
	Upcoming []Upcoming `json:"upcoming"`
}

// Upcoming is one airing a recording will capture.
type Upcoming struct {
	Title  string `json:"title"`
	Status string `json:"status"` // scheduled, inprogress, ...
	Airing
}

// Record schedules a recording of the guide item guid (from Search or Guide).
func (c *Client) Record(ctx context.Context, guid string, o RecordOptions) (*Recording, error) {
	var mc mediaContainer
	if err := c.do(ctx, "GET", "/media/subscriptions/template?guid="+url.QueryEscape(guid), &mc); err != nil {
		return nil, err
	}
	var choices []templateChoice
	for _, t := range mc.MediaContainer.SubscriptionTemplate {
		choices = append(choices, t.MediaSubscription...)
	}
	if len(choices) == 0 {
		return nil, fmt.Errorf("plex offers no way to record %s", guid)
	}
	var ch *templateChoice
	var titles []string
	for i := range choices {
		titles = append(titles, choices[i].Title)
		if (choices[i].Type == 2) == o.Series && ch == nil {
			ch = &choices[i]
		}
	}
	if ch == nil {
		want := "a single recording"
		if o.Series {
			want = "a series recording"
		}
		return nil, fmt.Errorf("plex doesn't offer %s for this item (choices: %s)", want, strings.Join(titles, ", "))
	}

	prefs := map[string]string{}
	for _, s := range ch.Setting {
		prefs[s.ID] = settingValue(s.Value)
	}
	if _, ok := prefs["onlyNewAirings"]; ok {
		prefs["onlyNewAirings"] = "1"
		if o.AllAirings {
			prefs["onlyNewAirings"] = "0"
		}
	}
	prefs["startOffsetMinutes"] = strconv.Itoa(o.PaddingBefore)
	prefs["endOffsetMinutes"] = strconv.Itoa(o.PaddingAfter)
	if o.Channel != "" {
		v, err := pickEnum(ch.Setting, "lineupChannel", func(_, label string) bool {
			vcn, _, _ := strings.Cut(label, " ")
			return vcn == o.Channel
		})
		if err != nil {
			return nil, fmt.Errorf("channel %s: %w", o.Channel, err)
		}
		prefs["lineupChannel"] = v
	}
	if !o.Start.IsZero() {
		// Plex's choices are times of day, each given as one airing's
		// timestamp, so match the time of day.
		want := o.Start.Local().Format("15:04")
		v, err := pickEnum(ch.Setting, "startTimeslot", func(value, _ string) bool {
			n, err := strconv.ParseInt(value, 10, 64)
			return err == nil && n >= 0 && time.Unix(n, 0).Format("15:04") == want
		})
		if err != nil {
			return nil, fmt.Errorf("start time %s: %w", o.Start.Format(time.RFC3339), err)
		}
		prefs["startTimeslot"] = v
	}

	loc, err := c.sectionLocation(ctx, ch.TargetLibrarySectionID)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"targetLibrarySectionID":  {strconv.Itoa(ch.TargetLibrarySectionID)},
		"targetSectionLocationID": {strconv.Itoa(loc)},
		"includeGrabs":            {"1"},
		"type":                    {strconv.Itoa(ch.Type)},
	}
	for k, v := range prefs {
		q.Set("prefs["+k+"]", v)
	}
	var created mediaContainer
	if err := c.do(ctx, "POST", "/media/subscriptions?"+ch.Parameters+"&"+q.Encode(), &created); err != nil {
		return nil, err
	}
	if len(created.MediaContainer.MediaSubscription) == 0 {
		return nil, errors.New("plex accepted the recording but returned nothing")
	}
	r := created.MediaContainer.MediaSubscription[0].recording()
	return &r, nil
}

func pickEnum(settings []setting, id string, match func(value, label string) bool) (string, error) {
	for _, s := range settings {
		if s.ID != id {
			continue
		}
		var labels []string
		for _, e := range enumValues(s.EnumValues) {
			if e[0] != "" && match(e[0], e[1]) {
				return e[0], nil
			}
			labels = append(labels, e[1])
		}
		return "", fmt.Errorf("not one of the airings (choices: %s)", strings.Join(labels, ", "))
	}
	return "", errors.New("plex doesn't offer that choice for this item")
}

func (c *Client) sectionLocation(ctx context.Context, section int) (int, error) {
	var mc mediaContainer
	if err := c.do(ctx, "GET", "/library/sections", &mc); err != nil {
		return 0, err
	}
	for _, d := range mc.MediaContainer.Directory {
		if d.Key == strconv.Itoa(section) && len(d.Location) > 0 {
			return d.Location[0].ID, nil
		}
	}
	return 0, fmt.Errorf("library section %d has no folders", section)
}

// Plex metadata types used as subscription types.
var subscriptionKinds = map[int]string{1: "movie", 2: "series", 4: "episode"}

func (s mediaSubscription) recording() Recording {
	r := Recording{ID: s.Key, Kind: subscriptionKinds[s.Type], Library: s.LibrarySectionTitle, Upcoming: []Upcoming{}}
	if r.Kind == "" {
		r.Kind = s.Title
	}
	if m := s.Video; m != nil {
		r.Title, r.Show = m.Title, m.GrandparentTitle
	} else if m := s.Directory; m != nil {
		r.Title = m.Title
	}
	for _, g := range s.MediaGrabOperation {
		u := Upcoming{Title: g.Metadata.Title, Status: g.Status}
		if i := g.MediaIndex; i >= 0 && i < len(g.Metadata.Media) {
			u.Airing = g.Metadata.Media[i].airing()
		}
		r.Upcoming = append(r.Upcoming, u)
	}
	sort.Slice(r.Upcoming, func(i, j int) bool { return r.Upcoming[i].Start.Before(r.Upcoming[j].Start) })
	return r
}

// Recordings lists scheduled recordings.
func (c *Client) Recordings(ctx context.Context) ([]Recording, error) {
	var mc mediaContainer
	if err := c.do(ctx, "GET", "/media/subscriptions?includeGrabs=1", &mc); err != nil {
		return nil, err
	}
	out := []Recording{}
	for _, s := range mc.MediaContainer.MediaSubscription {
		out = append(out, s.recording())
	}
	return out, nil
}

// Cancel deletes a recording by ID.
func (c *Client) Cancel(ctx context.Context, id string) error {
	if _, err := strconv.Atoi(id); err != nil {
		return fmt.Errorf("bad recording id %q", id)
	}
	return c.do(ctx, "DELETE", "/media/subscriptions/"+id, nil)
}
