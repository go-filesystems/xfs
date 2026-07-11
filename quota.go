package filesystem_xfs

// Quota support — the classic (pre-metadir) on-disk quota layout.
//
// XFS accounts disk-block and inode usage per identity (user, group and/or
// project) in dedicated quota inodes referenced by sb_uquotino, sb_gquotino
// and sb_pquotino. Each quota inode is an ordinary regular-file inode whose
// data fork stores a dense array of on-disk quota records (xfs_dqblk). Record
// N describes identity id=N and lives at file offset N*sizeof(xfs_dqblk); the
// file is sparse/short until higher ids appear.
//
// This is the layout the kernel and older mkfs.xfs use when the metadata
// directory feature is off — the form xfs_repair validates structurally. We
// create the quota inodes at format time (mkfs -m uquota,gquota,pquota) and
// seed each with the dquot for id 0, matching what a freshly-checked
// filesystem carries.

// QuotaConfig selects which quota types a Format enables. The zero value
// leaves quotas off. Enforce additionally sets the *_ENFD (limit enforcement)
// flags; without it only accounting (*_ACCT) plus the *_CHKD "counts verified"
// flags are set, which is the state of a freshly quota-checked filesystem.
type QuotaConfig struct {
	User    bool // user quota accounting (sb_uquotino)
	Group   bool // group quota accounting (sb_gquotino)
	Project bool // project quota accounting (sb_pquotino)
	Enforce bool // also enforce limits (set *_ENFD flags)
}

// isZero reports whether no quota type is selected.
func (q QuotaConfig) isZero() bool { return !q.User && !q.Group && !q.Project }

// qflags computes the sb_qflags value for this configuration. Accounting and
// "quota checked" flags are always set for a selected type; enforcement flags
// are added only when Enforce is set.
func (q QuotaConfig) qflags() uint16 {
	var f uint16
	if q.User {
		f |= xfsUQuotaAcct | xfsUQuotaChkd
		if q.Enforce {
			f |= xfsUQuotaEnfd
		}
	}
	if q.Group {
		f |= xfsGQuotaAcct | xfsGQuotaChkd
		if q.Enforce {
			f |= xfsGQuotaEnfd
		}
	}
	if q.Project {
		f |= xfsPQuotaAcct | xfsPQuotaChkd
		if q.Enforce {
			f |= xfsPQuotaEnfd
		}
	}
	return f
}

// setupQuota is implemented in quota_setup.go.
