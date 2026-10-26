package server

import (
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/opencodehist"
	"cc-webui/internal/opencodev2"
	"cc-webui/internal/opencodever"
	"cc-webui/internal/store"
)

// ocIsV2 reports whether this cli's binary speaks the OpenCode 2 protocol.
// OpenCode 2 is a breaking server-API/CLI rewrite that is installed over the
// same path as v1, so the version is detected from the binary itself (with a
// config override) and every opencode read/live path routes through it.
func ocIsV2(cli *config.CLI) bool {
	return cli != nil && cli.IsOpenCode() && opencodever.IsV2(cli.Path, cli.OpenCodeAPI)
}

// ocList lists one project bucket, whichever protocol the binary speaks.
func ocList(cli *config.CLI, dir string) ([]opencodehist.SessionInfo, error) {
	if !ocIsV2(cli) {
		return opencodehist.List(cli.Path, dir)
	}
	rows, err := opencodev2.List(cli.Path, dir)
	if err != nil {
		return nil, err
	}
	return toHistInfo(rows), nil
}

// ocListAll lists every top-level session across projects.
func ocListAll(cli *config.CLI) ([]opencodehist.SessionInfo, error) {
	if !ocIsV2(cli) {
		return opencodehist.ListAll(cli.Path)
	}
	rows, err := opencodev2.ListAll(cli.Path)
	if err != nil {
		return nil, err
	}
	return toHistInfo(rows), nil
}

func toHistInfo(rows []opencodev2.SessionInfo) []opencodehist.SessionInfo {
	out := make([]opencodehist.SessionInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, opencodehist.SessionInfo{
			SessionID: r.SessionID, Title: r.Title, Cwd: r.Cwd,
			UpdatedAt: r.UpdatedAt, CreatedAt: r.CreatedAt,
		})
	}
	return out
}

// ocReplayEvents converts a session's transcript into canonical replay events.
func ocReplayEvents(cli *config.CLI, id string) ([]*store.Event, error) {
	if !ocIsV2(cli) {
		s, err := opencodehist.FetchSession(cli.Path, id)
		if err != nil {
			return nil, err
		}
		return s.ToEvents(), nil
	}
	s, err := opencodev2.FetchSession(cli.Path, id)
	if err != nil {
		return nil, err
	}
	return s.ToEvents(), nil
}

// ocImportModel returns the model a session ran on (for import alignment) and
// doubles as the readability check.
func ocImportModel(cli *config.CLI, id string) (string, error) {
	if !ocIsV2(cli) {
		s, err := opencodehist.FetchSession(cli.Path, id)
		if err != nil {
			return "", err
		}
		return s.ModelID, nil
	}
	s, err := opencodev2.FetchSession(cli.Path, id)
	if err != nil {
		return "", err
	}
	return s.ModelID, nil
}

// ocPreview returns the import-preview fields (id, cwd, updated, messages).
func ocPreview(cli *config.CLI, id string) (string, string, time.Time, []opencodehist.PreviewMessage, error) {
	if !ocIsV2(cli) {
		s, err := opencodehist.FetchSession(cli.Path, id)
		if err != nil {
			return "", "", time.Time{}, nil, err
		}
		return s.ID, s.Directory, s.UpdatedAt, s.Preview(8), nil
	}
	s, err := opencodev2.FetchSession(cli.Path, id)
	if err != nil {
		return "", "", time.Time{}, nil, err
	}
	msgs := make([]opencodehist.PreviewMessage, 0, 8)
	for _, m := range s.Preview(8) {
		msgs = append(msgs, opencodehist.PreviewMessage{Role: m.Role, Text: m.Text, TS: m.TS})
	}
	return s.ID, s.Directory, s.UpdatedAt, msgs, nil
}

// readOpenCodeCatalog reads a provider's model catalog for the binary's
// protocol, normalizing onto opencodehist.Catalog so the memo and the two
// endpoints stay protocol-agnostic.
func readOpenCodeCatalog(cli *config.CLI, providerID string, refresh bool) (*opencodehist.Catalog, error) {
	if !ocIsV2(cli) {
		return opencodehist.ProviderCatalog(cli.Path, providerID, refresh)
	}
	c, err := opencodev2.ProviderCatalog(cli.Path, providerID, refresh)
	if err != nil {
		return nil, err
	}
	out := &opencodehist.Catalog{Variants: c.Variants}
	for _, m := range c.Models {
		out.Models = append(out.Models, opencodehist.Model{ID: m.ID, Input: m.Input})
	}
	return out, nil
}
