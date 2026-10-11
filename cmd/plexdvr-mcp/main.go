// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Command plexdvr-mcp is an MCP server (stdio) that lets AI agents search the
// Plex Live TV guide and schedule, list and cancel DVR recordings.
//
// It talks to the Plex server on this Mac. PLEX_URL and PLEX_TOKEN override
// the address and token; by default the token is read from Plex Media
// Server's preferences.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mattrasband/dualhd-mac/plexdvr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is set at build time by scripts/build-release.sh.
var version = "dev"

const instructions = `Schedules TV recordings on the user's Plex DVR (over-the-air antenna).
Find the program with search_guide (by title) or whats_on (by time/channel),
then call record with its guid. Times are in the server's local time zone;
each result includes "now" for reference. Sports games are episodes of a
show such as "NFL Football" or "College Football", and are found by team name.`

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log.SetOutput(os.Stderr) // stdout carries the MCP protocol

	plex, err := plexdvr.NewLocal()
	if err != nil {
		log.Fatal(err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "plexdvr", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	addTools(s, plex)
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

func now() string { return time.Now().Format(time.RFC3339) }

type searchIn struct {
	Query string `json:"query" jsonschema:"title, team or keywords, e.g. \"Jeopardy\" or \"Broncos\""`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results per kind (shows, episodes, movies); default 10"`
}

type searchOut struct {
	Now      string            `json:"now"`
	Programs []plexdvr.Program `json:"programs"`
}

type whatsOnIn struct {
	Start   string  `json:"start,omitempty" jsonschema:"start of the window, RFC 3339 (e.g. 2026-10-11T20:00:00-06:00); default now"`
	Hours   float64 `json:"hours,omitempty" jsonschema:"length of the window in hours; default 3, at most 12 (at most 4 without a channel)"`
	Channel string  `json:"channel,omitempty" jsonschema:"only this virtual channel, e.g. \"9.1\", or \"9\" for all of 9.x"`
	Query   string  `json:"query,omitempty" jsonschema:"only programs whose title or show contains this text (case-insensitive)"`
}

type whatsOnOut struct {
	Now     string               `json:"now"`
	Airings []plexdvr.GuideEntry `json:"airings"`
}

type recordIn struct {
	GUID           string `json:"guid" jsonschema:"guid of a show, episode or movie from search_guide or whats_on"`
	Series         bool   `json:"series,omitempty" jsonschema:"record every episode of the show instead of just this episode or movie"`
	IncludeRepeats bool   `json:"include_repeats,omitempty" jsonschema:"series only: also record repeats, not just new episodes"`
	Channel        string `json:"channel,omitempty" jsonschema:"only record on this virtual channel, e.g. \"9.1\""`
	Start          string `json:"start,omitempty" jsonschema:"only record airings starting at this time of day: the start (RFC 3339) of the airing to record, copied from search results. With channel, picks which airing a single recording captures (the first airing at that channel and time of day)"`
	PaddingBefore  int    `json:"padding_before_minutes,omitempty" jsonschema:"minutes to start early"`
	PaddingAfter   int    `json:"padding_after_minutes,omitempty" jsonschema:"minutes to keep recording after the scheduled end (useful for live sports)"`
}

type recordingsOut struct {
	Now        string              `json:"now"`
	Recordings []plexdvr.Recording `json:"recordings"`
}

type cancelIn struct {
	ID string `json:"id" jsonschema:"recording id from list_recordings or record"`
}

type cancelOut struct {
	Cancelled string `json:"cancelled"`
}

func addTools(s *mcp.Server, plex *plexdvr.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_guide",
		Description: "Search the TV guide by title. Returns shows, and upcoming episodes and movies with their airings (channel, start, end).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
		if strings.TrimSpace(in.Query) == "" {
			return nil, searchOut{}, errors.New("query is required")
		}
		if in.Limit <= 0 {
			in.Limit = 10
		}
		p, err := plex.Search(ctx, in.Query, min(in.Limit, 50))
		if p == nil {
			p = []plexdvr.Program{}
		}
		return nil, searchOut{now(), p}, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "whats_on",
		Description: "List what airs in a time window, optionally on one channel or matching a title.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in whatsOnIn) (*mcp.CallToolResult, whatsOnOut, error) {
		from := time.Now()
		if in.Start != "" {
			t, err := time.Parse(time.RFC3339, in.Start)
			if err != nil {
				return nil, whatsOnOut{}, fmt.Errorf("start: %w", err)
			}
			from = t
		}
		maxHours := 4.0
		if in.Channel != "" || in.Query != "" {
			maxHours = 12
		}
		if in.Hours <= 0 {
			in.Hours = 3
		}
		in.Hours = min(in.Hours, maxHours)
		to := from.Add(time.Duration(in.Hours * float64(time.Hour)))
		all, err := plex.Guide(ctx, from, to, in.Channel)
		if err != nil {
			return nil, whatsOnOut{}, err
		}
		out := whatsOnOut{Now: now(), Airings: []plexdvr.GuideEntry{}}
		q := strings.ToLower(in.Query)
		for _, e := range all {
			if q == "" || strings.Contains(strings.ToLower(e.Title+" "+e.Show), q) {
				out.Airings = append(out.Airings, e)
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "record",
		Description: "Schedule a DVR recording of a guide item: one episode, sports event or movie, or (series=true) every episode of a show. " +
			"A single recording captures the next airing unless channel/start pick a specific one. Returns the recording and the airings it will capture.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recordIn) (*mcp.CallToolResult, plexdvr.Recording, error) {
		if in.GUID == "" {
			return nil, plexdvr.Recording{}, errors.New("guid is required")
		}
		o := plexdvr.RecordOptions{
			Series: in.Series, AllAirings: in.IncludeRepeats, Channel: in.Channel,
			PaddingBefore: max(in.PaddingBefore, 0), PaddingAfter: max(in.PaddingAfter, 0),
		}
		if in.Start != "" {
			t, err := time.Parse(time.RFC3339, in.Start)
			if err != nil {
				return nil, plexdvr.Recording{}, fmt.Errorf("start: %w", err)
			}
			o.Start = t
		}
		r, err := plex.Record(ctx, in.GUID, o)
		if err != nil {
			return nil, plexdvr.Recording{}, err
		}
		return nil, *r, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_recordings",
		Description: "List scheduled recordings (one-off and series) with the airings each will capture.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, recordingsOut, error) {
		r, err := plex.Recordings(ctx)
		return nil, recordingsOut{now(), r}, err
	})

	destructive := true
	mcp.AddTool(s, &mcp.Tool{
		Name:        "cancel_recording",
		Description: "Cancel a scheduled recording (and, for a series, all its future episodes). Already-recorded shows are kept.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive, IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cancelIn) (*mcp.CallToolResult, cancelOut, error) {
		if err := plex.Cancel(ctx, in.ID); err != nil {
			return nil, cancelOut{}, err
		}
		return nil, cancelOut{in.ID}, nil
	})
}
