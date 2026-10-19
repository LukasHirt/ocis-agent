package store

// The kill switch is the system key KillSwitchKey; it is on exactly when its
// value is KillSwitchOnValue (an absent key is off). While it is on,
// ClaimNextJob hands out nothing, like for a paused user.
const (
	KillSwitchKey     = "killswitch"
	KillSwitchOnValue = "1"
)

// pauseRank orders the automatic pause reasons: a higher rank is more serious
// and replaces a lower one. The user's own pause is not ranked; see
// PauseSupersedes.
func pauseRank(reason string) int {
	switch reason {
	case PauseTokenAnomaly:
		return 5
	case PauseCredentialRevoked:
		return 4
	case PauseCredentialInvalid:
		return 3
	case PauseCredentialExpired:
		return 2
	case PauseDormant:
		return 1
	}
	return 0
}

// PauseSupersedes reports whether a pause for reason next should replace the
// current pause state (current is empty when the agent is not paused).
//
// The user's own pause is sticky: automatic reasons never replace it. A user
// pause does replace an automatic one. Among automatic reasons the higher
// rank wins (token_anomaly, credential_revoked, credential_invalid,
// credential_expired, dormant). Only dormant and token_anomaly are
// re-raised by periodic checks. The credential_* pauses are re-raised only
// when oCIS answers 401 for the stored credentials, so a caller (the API) must
// not replace one with a user pause: clearing it would lose the reason.
func PauseSupersedes(current, next string) bool {
	switch {
	case next == "":
		return false
	case current == "":
		return true
	case next == PauseUser:
		return current != PauseUser
	case current == PauseUser:
		return false
	default:
		return pauseRank(next) > pauseRank(current)
	}
}
