package acpview

import "github.com/BenedictKing/mobile-bridge/internal/jsoncompat"

// NativeSnapshot is one read of an agent's own session store (ZCode's SQLite
// state): the visible conversation as view-shaped turns plus the session
// title. It is applied wholesale so the view never has to reconcile two
// sources by message id.
type NativeSnapshot struct {
	Title string
	Turns []*jsoncompat.Object
}

// ApplyNative replaces the visible turns with a native snapshot and mirrors
// the tip's status onto the view. A snapshot without turns is a no-op so
// pollers do not signal empty sessions.
func (v *View) ApplyNative(snapshot *NativeSnapshot) bool {
	if v == nil || snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	turns := make([]*jsoncompat.Object, 0, len(snapshot.Turns))
	for _, turn := range snapshot.Turns {
		if turn == nil {
			continue
		}
		cloned, _ := jsoncompat.Clone(turn).(*jsoncompat.Object)
		if cloned != nil {
			turns = append(turns, cloned)
		}
	}
	if len(turns) == 0 {
		return false
	}
	if snapshot.Title != "" {
		v.Title = snapshot.Title
	}
	v.Turns = turns
	v.Status = "idle"
	if status, _ := turns[len(turns)-1].MustGet("status").(string); status == "inProgress" {
		v.Status = "active"
	}
	return true
}

// TipTurn returns the last turn without cloning it.
func (v *View) TipTurn() *jsoncompat.Object {
	if v == nil {
		return nil
	}
	return v.currentTurn()
}
