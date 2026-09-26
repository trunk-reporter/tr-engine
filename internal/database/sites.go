package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database/sqlcdb"
)

type Site struct {
	SiteID     int
	SystemID   int
	InstanceID string
	ShortName  string
	Sysid      string
}

// FindOrCreateSite ensures a site exists for the given system, instance, and short_name.
func (db *DB) FindOrCreateSite(ctx context.Context, systemID int, instanceID, shortName string) (int, error) {
	return db.Q.FindOrCreateSite(ctx, sqlcdb.FindOrCreateSiteParams{
		SystemID:   systemID,
		InstanceID: instanceID,
		ShortName:  shortName,
	})
}

// UpdateSite updates a site's P25 identity fields from system info messages.
func (db *DB) UpdateSite(ctx context.Context, siteID int, sysNum int, nac string, rfss int, p25SiteID int, systemTypeRaw string) error {
	return db.Q.UpdateSite(ctx, sqlcdb.UpdateSiteParams{
		SiteID:        siteID,
		SysNum:        int16(sysNum),
		Nac:           nac,
		Rfss:          int16(rfss),
		P25SiteID:     int16(p25SiteID),
		SystemTypeRaw: systemTypeRaw,
	})
}

// SiteAPI represents a site for API responses.
type SiteAPI struct {
	SiteID     int    `json:"site_id"`
	SystemID   int    `json:"system_id"`
	ShortName  string `json:"short_name"`
	InstanceID string `json:"instance_id"`
	Nac        string `json:"nac,omitempty"`
	Rfss       *int   `json:"rfss,omitempty"`
	P25SiteID  *int   `json:"p25_site_id,omitempty"`
	SysNum     *int   `json:"sys_num,omitempty"`
}

func siteRowToAPI(r sqlcdb.GetSiteByIDRow) SiteAPI {
	s := SiteAPI{
		SiteID:     r.SiteID,
		SystemID:   r.SystemID,
		ShortName:  r.ShortName,
		InstanceID: r.InstanceID,
		Nac:        r.Nac,
	}
	if r.Rfss != nil {
		v := int(*r.Rfss)
		s.Rfss = &v
	}
	if r.P25SiteID != nil {
		v := int(*r.P25SiteID)
		s.P25SiteID = &v
	}
	if r.SysNum != nil {
		v := int(*r.SysNum)
		s.SysNum = &v
	}
	return s
}

func listSiteRowToAPI(r sqlcdb.ListSitesForSystemRow) SiteAPI {
	s := SiteAPI{
		SiteID:     r.SiteID,
		SystemID:   r.SystemID,
		ShortName:  r.ShortName,
		InstanceID: r.InstanceID,
		Nac:        r.Nac,
	}
	if r.Rfss != nil {
		v := int(*r.Rfss)
		s.Rfss = &v
	}
	if r.P25SiteID != nil {
		v := int(*r.P25SiteID)
		s.P25SiteID = &v
	}
	if r.SysNum != nil {
		v := int(*r.SysNum)
		s.SysNum = &v
	}
	return s
}

func allSiteRowToAPI(r sqlcdb.LoadAllSitesAPIRow) SiteAPI {
	s := SiteAPI{
		SiteID:     r.SiteID,
		SystemID:   r.SystemID,
		ShortName:  r.ShortName,
		InstanceID: r.InstanceID,
		Nac:        r.Nac,
	}
	if r.Rfss != nil {
		v := int(*r.Rfss)
		s.Rfss = &v
	}
	if r.P25SiteID != nil {
		v := int(*r.P25SiteID)
		s.P25SiteID = &v
	}
	if r.SysNum != nil {
		v := int(*r.SysNum)
		s.SysNum = &v
	}
	return s
}

// GetSiteByID returns a single site. A site whose system p may not see is
// pgx.ErrNoRows, like one that doesn't exist (§6.3). A nil p is
// ErrNoPrincipal.
func (db *DB) GetSiteByID(ctx context.Context, p *auth.Principal, siteID int) (*SiteAPI, error) {
	if p == nil {
		return nil, ErrNoPrincipal
	}
	row, err := db.Q.GetSiteByID(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if !p.SystemVisible(row.SystemID) {
		return nil, pgx.ErrNoRows
	}
	s := siteRowToAPI(row)
	return &s, nil
}

// ListSitesForSystem returns all sites for a given system.
func (db *DB) ListSitesForSystem(ctx context.Context, systemID int) ([]SiteAPI, error) {
	rows, err := db.Q.ListSitesForSystem(ctx, systemID)
	if err != nil {
		return nil, err
	}
	sites := make([]SiteAPI, len(rows))
	for i, r := range rows {
		sites[i] = listSiteRowToAPI(r)
	}
	return sites, nil
}

// LoadAllSitesAPI returns all sites as SiteAPI structs.
func (db *DB) LoadAllSitesAPI(ctx context.Context) ([]SiteAPI, error) {
	rows, err := db.Q.LoadAllSitesAPI(ctx)
	if err != nil {
		return nil, err
	}
	sites := make([]SiteAPI, len(rows))
	for i, r := range rows {
		sites[i] = allSiteRowToAPI(r)
	}
	return sites, nil
}

// UpdateSiteFields updates mutable site fields. Only non-nil fields are updated.
func (db *DB) UpdateSiteFields(ctx context.Context, siteID int, shortName, instanceID, nac *string, rfss, p25SiteID *int) error {
	exists, err := db.Q.SiteExists(ctx, siteID)
	if err != nil || !exists {
		return fmt.Errorf("site not found")
	}

	snVal := ""
	if shortName != nil {
		snVal = *shortName
	}
	instVal := ""
	if instanceID != nil {
		instVal = *instanceID
	}
	nacVal := ""
	if nac != nil {
		nacVal = *nac
	}
	var rfssVal int16
	if rfss != nil {
		rfssVal = int16(*rfss)
	}
	var p25Val int16
	if p25SiteID != nil {
		p25Val = int16(*p25SiteID)
	}

	return db.Q.UpdateSiteFields(ctx, sqlcdb.UpdateSiteFieldsParams{
		SiteID:        siteID,
		ShortName:     snVal,
		NewInstanceID: instVal,
		Nac:           nacVal,
		Rfss:          rfssVal,
		P25SiteID:     p25Val,
	})
}

// SiteIDMap returns a map of site_id → Site for the given systems.
func (db *DB) SiteIDMap(ctx context.Context, systemIDs []int) (map[int]Site, error) {
	sites, err := db.LoadAllSites(ctx)
	if err != nil {
		return nil, err
	}
	idSet := make(map[int]bool, len(systemIDs))
	for _, id := range systemIDs {
		idSet[id] = true
	}
	m := make(map[int]Site)
	for _, s := range sites {
		if len(systemIDs) == 0 || idSet[s.SystemID] {
			m[s.SiteID] = s
		}
	}
	return m, nil
}

// LoadAllSites returns all sites.
func (db *DB) LoadAllSites(ctx context.Context) ([]Site, error) {
	rows, err := db.Q.LoadAllSites(ctx)
	if err != nil {
		return nil, err
	}
	sites := make([]Site, len(rows))
	for i, r := range rows {
		sites[i] = Site{
			SiteID:     r.SiteID,
			SystemID:   r.SystemID,
			InstanceID: r.InstanceID,
			ShortName:  r.ShortName,
			Sysid:      r.Sysid,
		}
	}
	return sites, nil
}
