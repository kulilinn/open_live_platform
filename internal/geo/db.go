package geo

import (
	"github.com/ip2location/ip2location-go/v9"
	"github.com/oschwald/maxminddb-golang"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
)

type Config struct {
	MMDB, V4, V6   string
	Filter, XPT    string
	TrustedProxies []netip.Prefix
}
type DB struct {
	mm     *maxminddb.Reader
	v4, v6 *ip2location.DB
	once   sync.Once
}

func Open(c Config) (*DB, error) {
	d := &DB{}
	var err error
	if c.MMDB != "" {
		d.mm, err = maxminddb.Open(c.MMDB)
		if err != nil {
			return nil, err
		}
	}
	if c.V4 != "" {
		d.v4, err = ip2location.OpenDB(c.V4)
	}
	if err == nil && c.V6 != "" {
		d.v6, err = ip2location.OpenDB(c.V6)
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}
func (d *DB) Close() {
	if d != nil {
		d.once.Do(func() {
			if d.mm != nil {
				d.mm.Close()
			}
			if d.v4 != nil {
				d.v4.Close()
			}
			if d.v6 != nil {
				d.v6.Close()
			}
		})
	}
}
func (d *DB) Country(ip netip.Addr) string {
	if !ip.IsValid() {
		return "-"
	}
	if d.mm != nil {
		var rec struct {
			Country string `maxminddb:"country_code"`
			GeoIP   struct {
				ISO string `maxminddb:"iso_code"`
			} `maxminddb:"country"`
		}
		if err := d.mm.Lookup(net.IP(ip.AsSlice()), &rec); err == nil {
			if rec.Country != "" {
				return rec.Country
			}
			if rec.GeoIP.ISO != "" {
				return rec.GeoIP.ISO
			}
		}
	}
	db := d.v6
	if ip.Unmap().Is4() {
		db = d.v4
	}
	if db != nil {
		if r, err := db.Get_country_short(ip.String()); err == nil && len(r.Country_short) == 2 {
			return r.Country_short
		}
	}
	return "-"
}
func FromEnv() (c Config) {
	path := func(key, fallback string) string {
		if p := os.Getenv(key); p != "" {
			return p
		}
		if _, err := os.Stat(fallback); err == nil {
			return fallback
		}
		return ""
	}
	c.MMDB = path("IPLOCATE", "data/ip-to-country.mmdb")
	c.V4 = path("IP2GEO_DBV4", "data/IP2LOCATION-LITE-DB11.BIN")
	c.V6 = path("IP2GEO_DBV6", "data/IP2LOCATION-LITE-DB11.IPV6.BIN")
	c.Filter, c.XPT = os.Getenv("COUNTY_FILTER"), os.Getenv("IP_CHECKER_XPT")
	for _, value := range strings.FieldsFunc(os.Getenv("TRUSTED_PROXY_CIDRS"), func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		prefix, err := netip.ParsePrefix(value)
		if err == nil {
			c.TrustedProxies = append(c.TrustedProxies, prefix)
		}
	}
	return
}
