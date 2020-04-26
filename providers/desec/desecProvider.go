package desec

import (
	"encoding/json"
	"fmt"

	"github.com/StackExchange/dnscontrol/v3/models"
	"github.com/StackExchange/dnscontrol/v3/pkg/diff"
	"github.com/StackExchange/dnscontrol/v3/pkg/printer"
	"github.com/StackExchange/dnscontrol/v3/providers"
	"github.com/miekg/dns/dnsutil"
)

/*
desec API DNS provider:
Info required in `creds.json`:
   - auth-token
*/

// NewDeSec creates the provider.
func NewDeSec(m map[string]string, metadata json.RawMessage) (providers.DNSServiceProvider, error) {
	c := &api{}
	c.creds.token = m["auth-token"]
	if c.creds.token == "" {
		return nil, fmt.Errorf("missing deSEC auth-token")
	}

	// Get a domain to validate authentication
	if err := c.fetchDomainList(); err != nil {
		return nil, err
	}

	return c, nil
}

var features = providers.DocumentationNotes{
	providers.DocDualHost:            providers.Unimplemented(),
	providers.DocOfficiallySupported: providers.Cannot(),
	providers.DocCreateDomains:       providers.Can(),
	providers.CanUseAlias:            providers.Cannot(),
	providers.CanUseSRV:              providers.Can(),
	providers.CanUseSSHFP:            providers.Cannot(),
	providers.CanUseCAA:              providers.Can(),
	providers.CanUseTLSA:             providers.Can(),
	providers.CanUsePTR:              providers.Unimplemented(),
	providers.CanGetZones:            providers.Can(),
	providers.CanAutoDNSSEC:          providers.Cannot(),
}

var defaultNameServerNames = []string{
	"ns1.desec.io",
	"ns2.desec.org",
}

func init() {
	providers.RegisterDomainServiceProviderType("DESEC", NewDeSec, features)
}

// GetNameservers returns the nameservers for a domain.
func (c *api) GetNameservers(domain string) ([]*models.Nameserver, error) {
	return models.ToNameservers(defaultNameServerNames)
}

func (c *api) GetDomainCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
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
func (c *api) GetZoneRecords(domain string) (models.Records, error) {
	records, err := c.getRecords(domain)
	if err != nil {
		return nil, err
	}

	// Convert them to DNScontrol's native format:
	existingRecords := []*models.RecordConfig{}
	for _, rr := range records {
		existingRecords = append(existingRecords, nativeToRecords(rr, domain)...)
	}
	return existingRecords, nil
}

// EnsureDomainExists returns an error if domain doesn't exist.
func (c *api) EnsureDomainExists(domain string) error {
	if err := c.fetchDomainList(); err != nil {
		return err
	}
	// domain already exists
	if _, ok := c.domainIndex[domain]; ok {
		return nil
	}
	return c.createDomain(domain)
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
	// Sort through the dc.Records, eliminate any that can't be
	// supported; modify any that need adjustments to work with the
	// provider.  We try to do minimal changes otherwise it gets
	// confusing.

	dc.Punycode()
	recordsToKeep := make([]*models.RecordConfig, 0, len(dc.Records))
	for _, rec := range dc.Records {
		if rec.Type == "ALIAS" {
			// deSEC does not permit ALIAS records, just ignore it
			printer.Warnf("deSEC does not support alias records\n")
			continue
		}
		if rec.TTL < 3600 {
			if rec.Type != "NS" {
				printer.Warnf("deSEC does not support ttls < 3600. Setting ttl of %s type %s from %d to 3600\n", rec.GetLabelFQDN(), rec.Type, rec.TTL)
			}
			rec.TTL = 3600
		}
		recordsToKeep = append(recordsToKeep, rec)
	}
	dc.Records = recordsToKeep
}

// GenerateDomainCorrections takes the desired and existing records
// and produces a Correction list.  The correction list is simply
// a list of functions to call to actually make the desired
// correction, and a message to output to the user when the change is
// made.
func (client *api) GenerateDomainCorrections(dc *models.DomainConfig, existing models.Records) ([]*models.Correction, error) {

	var corrections = []*models.Correction{}

	// diff existing vs. current.
	differ := diff.New(dc)
	keysToUpdate := differ.ChangedGroups(existing)
	if len(keysToUpdate) == 0 {
		return nil, nil
	}
	// Regroup data by FQDN.  ChangedGroups returns data grouped by label:RType tuples.
	//affectedLabels, msgsForLabel := gatherAffectedLabels(keysToUpdate)
	desiredRecords := dc.Records.GroupedByKey()
	//doesLabelExist := existing.FQDNMap()

	// For any key with an update, delete or replace those records.
	for label := range keysToUpdate {
		if _, ok := desiredRecords[label]; !ok {
			//we could not find this RecordKey in the desiredRecords
			//this means it must be deleted
			for i, msg := range keysToUpdate[label] {
				if i == 0 {
					//only the first call will actually delete all records
					corrections = append(corrections,
						&models.Correction{
							Msg: msg,
							F: func() error {
								shortname := dnsutil.TrimDomainName(label.NameFQDN, dc.Name)
								if shortname == "@" {
									shortname = ""
								}
								empty := make([]string, 0)
								rc := resourceRecord{
									Type:    label.Type,
									Subname: shortname,
									Records: empty,
								}
								err := client.deleteRR(rc, dc.Name)
								if err != nil {
									return err
								}
								return nil
							},
						})
				} else {
					//noop just for printing the additional messages
					corrections = append(corrections,
						&models.Correction{
							Msg: msg,
							F: func() error {
								return nil
							},
						})
				}
			}
		} else {
			//it must be an update or create, both can be done with the same api call.
			ns := recordsToNative(desiredRecords[label], dc.Name)
			if len(ns) > 1 {
				panic("we got more than one resource record to create / modify")
			}
			for i, msg := range keysToUpdate[label] {
				if i == 0 {
					corrections = append(corrections,
						&models.Correction{
							Msg: msg,
							F: func() error {
								rc := ns[0]
								err := client.upsertRR(rc, dc.Name)
								if err != nil {
									return err
								}
								return nil
							},
						})
				} else {
					//noop just for printing the additional messages
					corrections = append(corrections,
						&models.Correction{
							Msg: msg,
							F: func() error {
								return nil
							},
						})
				}
			}
		}
	}
	return corrections, nil
}
