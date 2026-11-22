package password

// Class says what a stored password_hash is good for.
type Class int

const (
	// ClassNone: no password set (NULL or empty hash).
	ClassNone Class = iota
	// ClassCurrent: argon2id at the current parameters.
	ClassCurrent
	// ClassLegacy: verifiable, but must be replaced on the next successful login.
	ClassLegacy
	// ClassUnsupported: unknown format or out-of-bounds parameters; needs reset.
	ClassUnsupported
)

func (c Class) String() string {
	switch c {
	case ClassNone:
		return "none"
	case ClassCurrent:
		return "current"
	case ClassLegacy:
		return "legacy"
	default:
		return "needs-reset"
	}
}

// Classify inspects the stored string only; it never hashes anything.
// Recognising a prefix is not support: a format counts as supported only when
// it parses within the bounds and has PHP-generated vectors (testdata/php).
func Classify(hash string) Class { return CurrentParams.Classify(hash) }

// Classify is Classify for a specific set of current parameters.
func (p Params) Classify(hash string) Class {
	if hash == "" {
		return ClassNone
	}
	if h, ok := parseArgon(hash); ok {
		if h.id && h.memory == p.Memory && h.time == p.Time && h.threads == p.Threads &&
			len(h.salt) == saltLen && len(h.key) == keyLen {
			return ClassCurrent
		}
		return ClassLegacy
	}
	if _, ok := bcryptCost(hash); ok {
		return ClassLegacy
	}
	return ClassUnsupported
}

// NeedsRehash reports whether a successful login must replace the hash
// (spec §4.3.4). A hash that is not supported never gets that far.
func NeedsRehash(hash string) bool { return Classify(hash) == ClassLegacy }
