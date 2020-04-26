package desec

// Convert the provider's native record description to models.RecordConfig.

import (
	"fmt"

	"github.com/StackExchange/dnscontrol/v3/models"
	"github.com/StackExchange/dnscontrol/v3/pkg/printer"
)

// nativeToRecord takes a DNS record from deSEC and returns a native RecordConfig struct.
func nativeToRecords(n resourceRecord, origin string) (rcs []*models.RecordConfig) {

	// deSEC returns all the values for a given label/rtype pair in each
	// livedns.DomainRecord.  In other words, if there are multiple A
	// records for a label, all the IP addresses are listed in
	// n.RrsetValues rather than having many livedns.DomainRecord's.
	// We must split them out into individual records, one for each value.
	for _, value := range n.Records {
		rc := &models.RecordConfig{
			TTL:      n.TTL,
			Original: n,
		}
		rc.SetLabel(n.Subname, origin)
		switch rtype := n.Type; rtype {
		default: //  "A", "AAAA", "CAA", "NS", "CNAME", "MX", "PTR", "SRV", "TXT"
			if err := rc.PopulateFromString(rtype, value, origin); err != nil {
				panic(fmt.Errorf("unparsable record received from deSEC: %w", err))
			}
		}
		rcs = append(rcs, rc)
	}

	return rcs
}

func recordsToNative(rcs []*models.RecordConfig, origin string) []resourceRecord {
	// Take a list of RecordConfig and return an equivalent list of ZoneRecords.
	// deSEC requires one ZoneRecord for each label:key tuple, therefore we
	// might collapse many RecordConfig into one ZoneRecord.

	var keys = map[models.RecordKey]*resourceRecord{}
	var zrs []resourceRecord

	for _, r := range rcs {
		label := r.GetLabel()
		if label == "@" {
			label = ""
		}
		key := r.Key()

		if zr, ok := keys[key]; !ok {
			// Allocate a new ZoneRecord:
			zr := resourceRecord{
				Type:    r.Type,
				TTL:     r.TTL,
				Subname: label,
				Records: []string{r.GetTargetCombined()},
			}
			zrs = append(zrs, zr)
			//keys[key] = &zr   // This didn't work.
			keys[key] = &zrs[len(zrs)-1] // This does work. I don't know why.

		} else {
			zr.Records = append(zr.Records, r.GetTargetCombined())

			if r.TTL != zr.TTL {
				printer.Warnf("All TTLs for a rrset (%v) must be the same. Using smaller of %v and %v.\n", key, r.TTL, zr.TTL)
				if r.TTL < zr.TTL {
					zr.TTL = r.TTL
				}
			}

		}
	}

	return zrs
}
