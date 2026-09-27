package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// ExportOptions configures what to export.
type ExportOptions struct {
	SystemIDs    []int      // filter to specific systems (empty = all)
	Version      string     // tr-engine version string
	Mode         string     // "metadata" or "full"
	IncludeAudio bool       // include audio files in archive
	Start        *time.Time // time range start for calls (optional)
	End          *time.Time // time range end for calls (optional)
	AudioDir     string     // path to audio directory on disk
}

// Export writes a tar.gz archive to w. Mode "metadata" exports systems, sites,
// talkgroups, units. Mode "full" also exports calls, transcriptions, and optionally audio.
func Export(ctx context.Context, db *database.DB, w io.Writer, opts ExportOptions) error {
	if opts.Mode == "" {
		opts.Mode = "metadata"
	}
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	// Load all systems with sites
	systems, err := db.ListSystemsWithSites(ctx, auth.Internal)
	if err != nil {
		return fmt.Errorf("load systems: %w", err)
	}

	// Filter by system IDs if specified
	if len(opts.SystemIDs) > 0 {
		idSet := make(map[int]bool, len(opts.SystemIDs))
		for _, id := range opts.SystemIDs {
			idSet[id] = true
		}
		filtered := systems[:0]
		for _, s := range systems {
			if idSet[s.SystemID] {
				filtered = append(filtered, s)
			}
		}
		systems = filtered
	}

	// Build system ID -> SystemRef map (needed by all entity exports)
	sysRefMap := make(map[int]SystemRef, len(systems))
	for _, s := range systems {
		sysRefMap[s.SystemID] = buildSystemRef(s)
	}

	systemIDs := make([]int, len(systems))
	for i, s := range systems {
		systemIDs[i] = s.SystemID
	}

	// Load all entity data
	allSites, err := db.LoadAllSites(ctx)
	if err != nil {
		return fmt.Errorf("load sites: %w", err)
	}
	talkgroups, err := db.ExportTalkgroups(ctx, systemIDs)
	if err != nil {
		return fmt.Errorf("load talkgroups: %w", err)
	}
	tgDir, err := db.ExportTalkgroupDirectory(ctx, systemIDs)
	if err != nil {
		return fmt.Errorf("load talkgroup directory: %w", err)
	}
	units, err := db.ExportUnits(ctx, systemIDs)
	if err != nil {
		return fmt.Errorf("load units: %w", err)
	}

	// Filter sites to selected systems
	var filteredSites []database.Site
	for _, s := range allSites {
		if _, ok := sysRefMap[s.SystemID]; ok {
			filteredSites = append(filteredSites, s)
		}
	}

	// Load full-mode data (calls, transcriptions) before writing manifest
	var calls []database.CallExport
	var transcriptions []database.TranscriptionExport
	var siteIDMap map[int]database.Site

	if opts.Mode == "full" {
		calls, err = db.ExportCalls(ctx, systemIDs, opts.Start, opts.End)
		if err != nil {
			return fmt.Errorf("load calls: %w", err)
		}

		siteIDMap, err = db.SiteIDMap(ctx, systemIDs)
		if err != nil {
			return fmt.Errorf("load site map: %w", err)
		}

		transcriptions, err = db.ExportTranscriptions(ctx, systemIDs, opts.Start, opts.End)
		if err != nil {
			return fmt.Errorf("load transcriptions: %w", err)
		}
	}

	// Write manifest
	manifest := Manifest{
		Version:        1,
		Format:         "tr-engine-export",
		CreatedAt:      time.Now().UTC(),
		SourceInstance: opts.Version,
		Filters: ManifestFilters{
			SystemIDs: opts.SystemIDs,
		},
		Counts: ManifestCounts{
			Systems:            len(systems),
			Sites:              len(filteredSites),
			Talkgroups:         len(talkgroups),
			TalkgroupDirectory: len(tgDir),
			Units:              len(units),
		},
	}

	if opts.Mode == "full" {
		manifest.Counts.Calls = len(calls)
		manifest.Counts.Transcriptions = len(transcriptions)
		if opts.Start != nil || opts.End != nil {
			manifest.Filters.TimeRange = &TimeRange{Start: opts.Start, End: opts.End}
		}
		manifest.Filters.IncludeAudio = opts.IncludeAudio
	}

	// Pre-count audio files that exist on disk (manifest needs this before writing)
	if opts.Mode == "full" && opts.IncludeAudio && opts.AudioDir != "" {
		for _, c := range calls {
			if c.AudioFilePath == "" {
				continue
			}
			diskPath := filepath.Join(opts.AudioDir, filepath.FromSlash(filepath.ToSlash(c.AudioFilePath)))
			if _, err := os.Stat(diskPath); err == nil {
				manifest.Counts.AudioFiles++
			}
		}
	}

	if err := writeJSON(tw, "manifest.json", manifest); err != nil {
		return err
	}

	// Write systems
	if err := writeJSONL(tw, "systems.jsonl", func(enc *json.Encoder) error {
		for _, s := range systems {
			if err := enc.Encode(buildSystemRecord(s)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Write sites
	if err := writeJSONL(tw, "sites.jsonl", func(enc *json.Encoder) error {
		for _, s := range filteredSites {
			ref, ok := sysRefMap[s.SystemID]
			if !ok {
				continue
			}
			rec := SiteRecord{
				V:          1,
				SystemRef:  ref,
				InstanceID: s.InstanceID,
				ShortName:  s.ShortName,
				Nac:        s.Sysid, // Site.Sysid is the NAC
			}
			if err := enc.Encode(rec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Write talkgroups
	if err := writeJSONL(tw, "talkgroups.jsonl", func(enc *json.Encoder) error {
		for _, tg := range talkgroups {
			ref, ok := sysRefMap[tg.SystemID]
			if !ok {
				continue
			}
			rec := TalkgroupRecord{
				V:              1,
				SystemRef:      ref,
				Tgid:           tg.Tgid,
				AlphaTag:       tg.AlphaTag,
				AlphaTagSource: tg.AlphaTagSource,
				Tag:            tg.Tag,
				Group:          tg.Group,
				Description:    tg.Description,
				Mode:           tg.Mode,
				Priority:       tg.Priority,
				FirstSeen:      tg.FirstSeen,
				LastSeen:       tg.LastSeen,
			}
			if err := enc.Encode(rec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Write talkgroup directory
	if err := writeJSONL(tw, "talkgroup_directory.jsonl", func(enc *json.Encoder) error {
		for _, td := range tgDir {
			ref, ok := sysRefMap[td.SystemID]
			if !ok {
				continue
			}
			rec := TalkgroupDirectoryRecord{
				V:           1,
				SystemRef:   ref,
				Tgid:        td.Tgid,
				AlphaTag:    td.AlphaTag,
				Mode:        td.Mode,
				Description: td.Description,
				Tag:         td.Tag,
				Category:    td.Category,
				Priority:    td.Priority,
			}
			if err := enc.Encode(rec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Write units
	if err := writeJSONL(tw, "units.jsonl", func(enc *json.Encoder) error {
		for _, u := range units {
			ref, ok := sysRefMap[u.SystemID]
			if !ok {
				continue
			}
			rec := UnitRecord{
				V:              1,
				SystemRef:      ref,
				UnitID:         u.UnitID,
				AlphaTag:       u.AlphaTag,
				AlphaTagSource: u.AlphaTagSource,
				FirstSeen:      u.FirstSeen,
				LastSeen:       u.LastSeen,

				RecorderAlphaTag:     u.RecorderAlphaTag,
				RecorderAlphaTagSeen: u.RecorderAlphaTagSeen,
				OTAAlphaTag:          u.OTAAlphaTag,
				OTAAlphaTagFirstSeen: u.OTAAlphaTagFirstSeen,
				OTAAlphaTagLastSeen:  u.OTAAlphaTagLastSeen,
			}
			if err := enc.Encode(rec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if opts.Mode == "full" {
		// Write calls.jsonl
		if err := writeJSONL(tw, "calls.jsonl", func(enc *json.Encoder) error {
			for _, c := range calls {
				ref, ok := sysRefMap[c.SystemID]
				if !ok {
					continue
				}
				rec := buildCallRecord(c, ref, siteIDMap)
				if err := enc.Encode(rec); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}

		// Write transcriptions.jsonl
		if err := writeJSONL(tw, "transcriptions.jsonl", func(enc *json.Encoder) error {
			for _, t := range transcriptions {
				ref, ok := sysRefMap[t.SystemID]
				if !ok {
					continue
				}
				rec := TranscriptionRecord{
					V:             1,
					SystemRef:     ref,
					Tgid:          t.Tgid,
					CallStartTime: t.CallStartTime,
					Text:          t.Text,
					Source:        t.Source,
					IsPrimary:     t.IsPrimary,
					Confidence:    t.Confidence,
					Language:      t.Language,
					Model:         t.Model,
					Provider:      t.Provider,
					WordCount:     t.WordCount,
					DurationMs:    t.DurationMs,
					ProviderMs:    t.ProviderMs,
					Words:         t.Words,
				}
				if err := enc.Encode(rec); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}

		// Write audio files
		if opts.IncludeAudio && opts.AudioDir != "" {
			for _, c := range calls {
				if c.AudioFilePath == "" {
					continue
				}
				relPath := filepath.ToSlash(c.AudioFilePath)
				diskPath := filepath.Join(opts.AudioDir, filepath.FromSlash(relPath))
				info, statErr := os.Stat(diskPath)
				if statErr != nil {
					continue // skip missing audio files silently
				}
				af, openErr := os.Open(diskPath)
				if openErr != nil {
					continue
				}
				hdr := &tar.Header{
					Name:    "audio/" + relPath,
					Size:    info.Size(),
					Mode:    0644,
					ModTime: info.ModTime(),
				}
				if err := tw.WriteHeader(hdr); err != nil {
					af.Close()
					return fmt.Errorf("write audio header %s: %w", relPath, err)
				}
				if _, err := io.Copy(tw, af); err != nil {
					af.Close()
					return fmt.Errorf("write audio %s: %w", relPath, err)
				}
				af.Close()
			}
		}
	}

	return nil
}

func buildSystemRef(s database.SystemAPI) SystemRef {
	if s.Sysid != "" && s.Sysid != "0" {
		return SystemRef{Sysid: s.Sysid, Wacn: s.Wacn}
	}
	return SystemRef{} // conventional — identified via sites
}

func buildCallRecord(c database.CallExport, sysRef SystemRef, siteIDMap map[int]database.Site) CallRecord {
	rec := CallRecord{
		V:             1,
		SystemRef:     sysRef,
		Tgid:          c.Tgid,
		StartTime:     c.StartTime,
		StopTime:      c.StopTime,
		Duration:      c.Duration,
		Freq:          c.Freq,
		FreqError:     c.FreqError,
		SignalDB:      c.SignalDB,
		NoiseDB:       c.NoiseDB,
		ErrorCount:    c.ErrorCount,
		SpikeCount:    c.SpikeCount,
		AudioType:     c.AudioType,
		AudioFilePath: filepath.ToSlash(c.AudioFilePath),
		AudioFileSize: c.AudioFileSize,
		Phase2TDMA:    c.Phase2TDMA,
		TDMASlot:      c.TDMASlot,
		Analog:        c.Analog,
		Conventional:  c.Conventional,
		Encrypted:     c.Encrypted,
		Emergency:     c.Emergency,
		SrcList:       c.SrcList,
		FreqList:      c.FreqList,
		MetadataJSON:  c.MetadataJSON,
		IncidentData:  c.IncidentData,
		InstanceID:    c.InstanceID,
	}
	if len(c.PatchedTgids) > 0 {
		rec.PatchedTgids = make([]int, len(c.PatchedTgids))
		for i, v := range c.PatchedTgids {
			rec.PatchedTgids[i] = int(v)
		}
	}
	if len(c.UnitIDs) > 0 {
		rec.UnitIDs = make([]int, len(c.UnitIDs))
		for i, v := range c.UnitIDs {
			rec.UnitIDs[i] = int(v)
		}
	}
	if c.SiteID != nil {
		if site, ok := siteIDMap[*c.SiteID]; ok {
			rec.SiteRef = &SiteRef{
				InstanceID: site.InstanceID,
				ShortName:  site.ShortName,
			}
		}
	}
	return rec
}

func buildSystemRecord(s database.SystemAPI) SystemRecord {
	rec := SystemRecord{
		V:     1,
		Type:  s.SystemType,
		Name:  s.Name,
		Sysid: s.Sysid,
		Wacn:  s.Wacn,
	}
	// Conventional systems carry inline site refs for identification
	if s.Sysid == "" || s.Sysid == "0" {
		for _, site := range s.Sites {
			rec.Sites = append(rec.Sites, SiteRef{
				InstanceID: site.InstanceID,
				ShortName:  site.ShortName,
			})
		}
	}
	return rec
}

// writeJSON writes a single JSON object as a tar entry.
func writeJSON(tw *tar.Writer, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", name, err)
	}
	hdr := &tar.Header{
		Name:    name,
		Size:    int64(len(data)),
		Mode:    0644,
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header %s: %w", name, err)
	}
	_, err = tw.Write(data)
	return err
}

// writeJSONL writes JSONL records as a tar entry. Buffers in memory because tar
// headers require size upfront. Fine for metadata (<10MB); future call exports
// will need streaming with temp files.
func writeJSONL(tw *tar.Writer, name string, fn func(enc *json.Encoder) error) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := fn(enc); err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	hdr := &tar.Header{
		Name:    name,
		Size:    int64(buf.Len()),
		Mode:    0644,
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header %s: %w", name, err)
	}
	_, err := tw.Write(buf.Bytes())
	return err
}
