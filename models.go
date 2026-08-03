package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The model catalogue.
//
// Fetched from the web endpoint rather than through the SDK: the SDK's
// /v1/models is the OpenAI-compatible shape and carries only an id and a
// name, while what a picker needs is the context window, what the model is
// good at, and whether the account can reach it yet. Listing a model without
// saying "1M context" or "opens in August" is listing an id and hoping.

type modelInfo struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Tier          string     `json:"tier"`
	Tagline       string     `json:"tagline"`
	Description   string     `json:"description"`
	ContextLength int        `json:"context_length"`
	Available     bool       `json:"available"`
	AvailableFrom *time.Time `json:"available_from"`
}

// tagline is the one line under the name, falling back through what the
// catalogue happens to carry.
func (m modelInfo) tagline() string {
	switch {
	case m.Tagline != "":
		return m.Tagline
	case m.Description != "":
		return m.Description
	case m.Tier != "":
		return m.Tier
	default:
		return m.Name
	}
}

func fetchModels(ctx context.Context, c *credentials) ([]modelInfo, error) {
	var out struct {
		Models []modelInfo `json:"models"`
	}
	// The rich catalogue lives above /v1 — it is a web-app endpoint, not part
	// of the OpenAI-compatible surface.
	if err := getJSON(ctx, c, baseURLOf(c)+"/models", &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// pickModel lists the catalogue when given no argument, and otherwise
// switches to the named model. The list is fetched live so it cannot drift
// from what the account can actually reach; a bare "cipher" works.
//
// With no argument it also offers to switch, so choosing a model is one
// command rather than "list, read, retype".
func pickModel(c *credentials, current, want string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sp := startSpinner("loading models")
	models, err := fetchModels(ctx, c)
	sp.Stop()

	if err != nil {
		say("  " + paint(cErr, "✗ "+err.Error()))
		return current
	}
	if len(models) == 0 {
		say("  " + dim("no models available on this account"))
		return current
	}

	if want != "" {
		return resolveModelChoice(models, current, want)
	}

	say("")
	say("  " + paint(cUser, "Available Models"))
	say("")
	for i, m := range models {
		marker, id := "  ", m.ID
		switch {
		case !m.Available:
			// An announced-but-not-open model is listed rather than hidden:
			// "when can I use it" is a real question, and an absent row
			// answers it with silence.
			marker, id = "🔒 ", dim(m.ID)
		case m.ID == current:
			marker = paint(cOK, "● ")
		}

		say("  " + paint(cUser, strconv.Itoa(i+1)) + " " + marker + id)
		say("      " + dim(m.tagline()))
		if !m.Available && m.AvailableFrom != nil {
			say("      " + dim("Available: "+m.AvailableFrom.Format("2 January 2006")))
		}
		say("      " + dim("Context: "+humanContext(m.ContextLength)))
		say("")
	}

	fmt.Print("  " + paint(cUser, "Select ›") + " ")
	if !stdin.Scan() {
		return current
	}
	choice := cleanInput(stdin.Text())
	if choice == "" {
		return current
	}
	// A number picks from the list; anything else is treated as a name, so
	// answering "cipher" behaves exactly like "/model cipher".
	if n, convErr := strconv.Atoi(choice); convErr == nil && n >= 1 && n <= len(models) {
		return acceptModel(models[n-1], current)
	}
	return resolveModelChoice(models, current, choice)
}

func resolveModelChoice(models []modelInfo, current, want string) string {
	want = strings.TrimSpace(want)
	for _, m := range models {
		if m.ID == want || strings.TrimPrefix(m.ID, "measyai/") == want {
			return acceptModel(m, current)
		}
	}
	say("  " + paint(cErr, "✗ no model called "+want) + dim("  (/model lists them)"))
	return current
}

// acceptModel refuses a model the account cannot reach yet, rather than
// switching to it and letting the next prompt fail with a 4xx nobody can
// trace back to this moment.
func acceptModel(m modelInfo, current string) string {
	if !m.Available {
		msg := "✗ " + m.ID + " is not available yet"
		if m.AvailableFrom != nil {
			msg += " — opens " + m.AvailableFrom.Format("2 January 2006")
		}
		say("  " + paint(cErr, msg))
		return current
	}
	say("  " + paint(cOK, "✓") + "  model " + dim("→ ") + paint(cUser, m.ID))
	return m.ID
}

// humanContext renders 256000 as "256k" — the exact number is noise, the
// order of magnitude is the whole point.
func humanContext(n int) string {
	switch {
	case n <= 0:
		return "unknown"
	case n >= 1_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}
