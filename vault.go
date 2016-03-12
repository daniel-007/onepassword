package onepassword

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os/user"
	"path"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const (
	DefaultProfile    = "default"
	// Relative to user's home dir
	RelativeVaultPath = "Library/Containers/2BUA8C4S2C.com.agilebits.onepassword-osx-helper/Data/Library/Data/OnePassword.sqlite"
)

// A Vault is a read-only interface to the 1Password SQLite database.
type Vault struct {
	db          *sql.DB
	profileId   int
	masterKP    *KeyPair           // Encrypts item keypairs
	overviewKP  *KeyPair           // Encrypts overviews
	categories  map[string]string  // For uuid -> name
}

type VaultConfig struct {
	DBPath  string  // Path to the sqlite file
	Profile string  // Name of 1p profile
}

// Merge merges the two configs together. Properties in other take precedence
// over properties in cfg.
func (cfg *VaultConfig) Merge(other *VaultConfig) (*VaultConfig) {
	ret := &VaultConfig{}

	if other == nil {
		other = &VaultConfig{}
	}

	ret.DBPath = cfg.DBPath
	if other.DBPath != "" {
		ret.DBPath = other.DBPath
	}

	ret.Profile = cfg.Profile
	if other.Profile != "" {
		ret.Profile = other.Profile
	}

	return ret
}

func resolveDefaultDBPath() string {
	u, err := user.Current()
	if err != nil {
		panic(fmt.Sprintf("Cannot resolve current user: %s", err))
	}

	return path.Join(u.HomeDir, RelativeVaultPath)
}

var DefaultVaultConfig = &VaultConfig{
	DBPath: resolveDefaultDBPath(),
	Profile: DefaultProfile,
}

func getCategories(db *sql.DB, profileId int) (map[string]string, error) {
	cats := make(map[string]string)
	err := transact(db, func(tx *sql.Tx) (e error) {
		rows, e := tx.Query(
			"SELECT uuid, singular_name" +
			" FROM categories" +
			" WHERE profile_id = ?",
			profileId)
		if e != nil {
			return
		}
		defer rows.Close()

		// Fill in cats
		for rows.Next() {
			var uuid, name string
			e = rows.Scan(&uuid, &name)
			if e != nil {
				return
			}

			cats[uuid] = name
		}

		return rows.Err()
	})

	if err != nil {
		cats = nil
	}

	return cats, err
}

func NewVault(masterPass string, ucfg *VaultConfig) (*Vault, error) {
	cfg := DefaultVaultConfig.Merge(ucfg)
	db, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		return nil, err
	}

	// Lookup profile
	var profileId, nIters int
	var salt, masterKeyBlob, overviewKeyBlob []byte
	err = transact(db, func(tx *sql.Tx) error {
		row := tx.QueryRow(
			"SELECT id, iterations, master_key_data, overview_key_data, salt" +
			" FROM profiles" +
			" WHERE profile_name = ?",
			cfg.Profile)
		e := row.Scan(&profileId, &nIters, &masterKeyBlob, &overviewKeyBlob, &salt)
		if e == sql.ErrNoRows {
			e = fmt.Errorf("no profile named %q", cfg.Profile)
		}
		return e
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	// Decrypt master/overview keypairs
	derKP := ComputeDerivedKeys(masterPass, salt, nIters)
	mkp, err := DecryptMasterKeys(masterKeyBlob, derKP)
	if err != nil {
		db.Close()
		return nil, err
	}
	okp, err := DecryptMasterKeys(overviewKeyBlob, derKP)
	if err != nil {
		db.Close()
		return nil, err
	}

	// Get category index
	cats, err := getCategories(db, profileId)
	if err != nil {
		db.Close()
		return nil, err
	}

	v := &Vault{
		db: db,
		profileId: profileId,
		masterKP: mkp,
		overviewKP: okp,
		categories: cats,
	}

	return v, nil
}

type matchedItem struct {
	overview ItemOverview
	kp       *KeyPair
}

// LookupItems finds items in the 1Password database that match the supplied predicate.
func (v *Vault) LookupItems(pred ItemOverviewPredicate) ([]Item, error) {
	var items []Item

	err := transact(v.db, func(tx *sql.Tx) (e error) {
		rows, e := tx.Query(
			"SELECT id, category_uuid, key_data, overview_data" +
			" FROM items" +
			" WHERE profile_id = ? AND trashed = 0",
			v.profileId)
		if e != nil {
			return
		}
		defer rows.Close()

		// Figure out matches
		matchIds := make([]int, 0, 10)
		matches := make(map[int]*matchedItem)
		for rows.Next() {
			var itemId int
			var catUuid string
			var itemKeyBlob, opdata []byte
			e = rows.Scan(&itemId, &catUuid, &itemKeyBlob, &opdata)
			if e != nil {
				return
			}

			// Decrypt the overview
			var overview []byte
			overview, e = DecryptOPData01(opdata, v.overviewKP)
			if e != nil {
				return
			}

			// Check for a match
			var iov ItemOverview
			e = json.Unmarshal(overview, &iov)
			if e != nil {
				return
			}
			iov.Cat = Category{catUuid, v.categories[catUuid]}

			if pred(&iov) {
				// Decrypt the item key
				var kp *KeyPair
				kp, e = DecryptItemKey(itemKeyBlob, v.masterKP)
				if e != nil {
					return
				}
				matchIds = append(matchIds, itemId)
				matches[itemId] = &matchedItem{iov, kp}
			}
		}
		e = rows.Err()
		if e != nil {
			return
		}

		// Ughhhhhh ... grab match details
		var qargs []interface{}
		var qs []string
		for _, id := range(matchIds) {
			qs = append(qs, "?")
			qargs = append(qargs, id)
		}
		query := fmt.Sprintf(
			"SELECT item_id, data" +
			" FROM item_details" +
			" WHERE item_id IN (%s)",
			strings.Join(qs, ", "))
		detRows, e := tx.Query(query, qargs...)
		if e != nil {
			return
		}
		defer detRows.Close()

		// Decrypt match details and fill in items
		for detRows.Next() {
			var itemId int
			var opdata []byte
			e = detRows.Scan(&itemId, &opdata)
			if e != nil {
				return
			}

			// Decrypt the details
			var details []byte
			details, e = DecryptOPData01(opdata, matches[itemId].kp)
			if e != nil {
				return
			}

			items = append(items, Item{matches[itemId].overview, details})
		}
		e = detRows.Err()
		if e != nil {
			return
		}


		return nil
	})

	if err != nil {
		items = nil
	}

	return items, err
}

func (v *Vault) Close() {
	v.db.Close()
}
