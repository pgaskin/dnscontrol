package gcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/StackExchange/dnscontrol/v3/models"
	"github.com/StackExchange/dnscontrol/v3/pkg/diff"
	"github.com/StackExchange/dnscontrol/v3/providers"

	dnssdk "github.com/G-Core/gcore-dns-sdk-go"
)

/*
G-Core API DNS provider:
Info required in `creds.json`:
   - api-key
*/

type gcoreProvider struct {
	provider *dnssdk.Client
	ctx      context.Context
}

// NewGCore creates the provider.
func NewGCore(m map[string]string, metadata json.RawMessage) (providers.DNSServiceProvider, error) {
	if m["api-key"] == "" {
		return nil, fmt.Errorf("missing G-Core API key")
	}

	c := &gcoreProvider{
		provider: dnssdk.NewClient(dnssdk.PermanentAPIKeyAuth(m["api-key"])),
		ctx:      context.TODO(),
	}

	return c, nil
}

var features = providers.DocumentationNotes{
	providers.CanAutoDNSSEC:          providers.Cannot(),
	providers.CanGetZones:            providers.Can(),
	providers.CanUseAlias:            providers.Cannot(),
	providers.CanUseCAA:              providers.Can(),
	providers.CanUseDS:               providers.Cannot(),
	providers.CanUseNAPTR:            providers.Cannot(),
	providers.CanUsePTR:              providers.Cannot(),
	providers.CanUseSRV:              providers.Can("G-Core doesn't support SRV records with empty targets"),
	providers.CanUseSSHFP:            providers.Cannot(),
	providers.CanUseTLSA:             providers.Cannot(),
	providers.DocCreateDomains:       providers.Can(),
	providers.DocDualHost:            providers.Can(),
	providers.DocOfficiallySupported: providers.Cannot(),
}

var defaultNameServerNames = []string{
	"ns1.gcorelabs.net",
	"ns2.gcdn.services",
}

func init() {
	fns := providers.DspFuncs{
		Initializer:   NewGCore,
		RecordAuditor: AuditRecords,
	}
	providers.RegisterDomainServiceProviderType("GCORE", fns, features)
}

// GetNameservers returns the nameservers for a domain.
func (c *gcoreProvider) GetNameservers(domain string) ([]*models.Nameserver, error) {
	return models.ToNameservers(defaultNameServerNames)
}

func (c *gcoreProvider) GetDomainCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	existing, err := c.GetZoneRecords(dc.Name)
	if err != nil {
		return nil, err
	}
	models.PostProcessRecords(existing)
	clean := PrepFoundRecords(existing)
	PrepDesiredRecords(dc)
	return c.GenerateDomainCorrections(dc, clean)
}

// GetZoneRecords gets the records of a zone and returns them in RecordConfig format.
func (c *gcoreProvider) GetZoneRecords(domain string) (models.Records, error) {
	zone, err := c.provider.Zone(c.ctx, domain)
	if err != nil {
		return nil, err
	}

	// Convert RRsets to DNSControl format on the fly
	existingRecords := []*models.RecordConfig{}

	// We cannot directly use Zone's ShortAnswers
	// they aren't complete for CAA & SRV
	for _, rec := range zone.Records {
		rrset, err := c.provider.RRSet(c.ctx, zone.Name, rec.Name, rec.Type)
		if err != nil {
			return nil, err
		}
		nativeRecords, err := nativeToRecords(rrset, zone.Name, rec.Name, rec.Type)
		if err != nil {
			return nil, err
		}
		existingRecords = append(existingRecords, nativeRecords...)
	}

	return existingRecords, nil
}

// EnsureDomainExists returns an error if domain doesn't exist.
func (c *gcoreProvider) EnsureDomainExists(domain string) error {
	zones, err := c.provider.Zones(c.ctx)
	if err != nil {
		return err
	}

	for _, zone := range zones {
		if zone.Name == domain {
			return nil
		}
	}

	_, err = c.provider.CreateZone(c.ctx, domain)
	return err
}

// PrepFoundRecords munges any records to make them compatible with
// this provider. Usually this is a no-op.
func PrepFoundRecords(recs models.Records) models.Records {
	// If there are records that need to be modified, removed, etc. we
	// do it here.  Usually this is a no-op.
	return recs
}

// PrepDesiredRecords munges any records to best suit this provider.
func PrepDesiredRecords(dc *models.DomainConfig) {
	dc.Punycode()
}

func generateChangeMsg(updates []string) string {
	return strings.Join(updates, "\n")
}

// GenerateDomainCorrections takes the desired and existing records
// and produces a Correction list.  The correction list is simply
// a list of functions to call to actually make the desired
// correction, and a message to output to the user when the change is
// made.
func (c *gcoreProvider) GenerateDomainCorrections(dc *models.DomainConfig, existing models.Records) ([]*models.Correction, error) {

	var corrections = []*models.Correction{}

	// diff existing vs. current.
	differ := diff.New(dc)
	keysToUpdate, err := differ.ChangedGroups(existing)
	if err != nil {
		return nil, err
	}
	if len(keysToUpdate) == 0 {
		return nil, nil
	}

	desiredRecords := dc.Records.GroupedByKey()
	existingRecords := existing.GroupedByKey()

	// First pass: delete records to avoid coexisting of conflicting types
	for label := range keysToUpdate {
		if _, ok := desiredRecords[label]; !ok {
			// record deleted in update
			// Copy all params to avoid overwrites
			zone := dc.Name
			name := label.NameFQDN
			typ := label.Type
			msg := generateChangeMsg(keysToUpdate[label])
			corrections = append(corrections, &models.Correction{
				Msg: msg,
				F: func() error {
					return c.provider.DeleteRRSet(c.ctx, zone, name, typ)
				},
			})
		}
	}

	// Second pass: create and update records
	for label := range keysToUpdate {
		if _, ok := desiredRecords[label]; !ok {
			// record deleted in update
			// do nothing here

		} else if _, ok := existingRecords[label]; !ok {
			// record created in update
			record := recordsToNative(desiredRecords[label], label)
			if record == nil {
				panic("No records matching label")
			}

			// Copy all params to avoid overwrites
			zone := dc.Name
			name := label.NameFQDN
			typ := label.Type
			rec := *record
			msg := generateChangeMsg(keysToUpdate[label])
			corrections = append(corrections, &models.Correction{
				Msg: msg,
				F: func() error {
					return c.provider.CreateRRSet(c.ctx, zone, name, typ, rec)
				},
			})

		} else {
			// record modified in update
			record := recordsToNative(desiredRecords[label], label)
			if record == nil {
				panic("No records matching label")
			}

			// Copy all params to avoid overwrites
			zone := dc.Name
			name := label.NameFQDN
			typ := label.Type
			rec := *record
			msg := generateChangeMsg(keysToUpdate[label])
			corrections = append(corrections, &models.Correction{
				Msg: msg,
				F: func() error {
					return c.provider.UpdateRRSet(c.ctx, zone, name, typ, rec)
				},
			})
		}
	}

	return corrections, nil
}
