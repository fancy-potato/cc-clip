package x11bridge

import (
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// handleTargetsRequest responds to a TARGETS request by listing available
// clipboard formats. It probes the remote clipboard to determine whether
// image data is available.
func handleTargetsRequest(
	conn *xgb.Conn,
	event xproto.SelectionRequestEvent,
	atoms *AtomCache,
	hasImage bool,
) error {
	targetsAtom := atoms.MustGet(AtomNameTargets)
	timestampAtom, _ := atoms.Get(AtomNameTimestamp)

	targets := []xproto.Atom{targetsAtom, timestampAtom}

	if hasImage {
		imagePNG := atoms.MustGet(AtomNameImagePNG)
		targets = append(targets, imagePNG)
	}

	// Write the list of target atoms to the requestor's property.
	data := atomsToBytes(targets)
	err := xproto.ChangePropertyChecked(
		conn,
		xproto.PropModeReplace,
		event.Requestor,
		event.Property,
		xproto.AtomAtom, // type = ATOM
		32,              // format = 32 bits per element
		uint32(len(targets)),
		data,
	).Check()
	if err != nil {
		return err
	}

	return sendSelectionNotify(conn, event, event.Property)
}

// handleImageRequest responds to an image/png request by writing image data
// to the requestor's property. If the data exceeds maxDirectSize, it returns
// the data for INCR handling instead of writing directly.
//
// Returns (nil, nil) if data was written directly (no INCR needed).
// Returns (data, nil) if INCR is needed (caller must start INCR transfer).
// Returns (nil, err) on failure.
func handleImageRequest(
	conn *xgb.Conn,
	event xproto.SelectionRequestEvent,
	atoms *AtomCache,
	imageData []byte,
	maxDirectSize int,
) (incrData []byte, err error) {
	if len(imageData) == 0 {
		return nil, refuseRequest(conn, event)
	}

	imagePNG := atoms.MustGet(AtomNameImagePNG)

	if len(imageData) <= maxDirectSize {
		// Direct transfer: write all data to the requestor's property.
		err := xproto.ChangePropertyChecked(
			conn,
			xproto.PropModeReplace,
			event.Requestor,
			event.Property,
			imagePNG,
			8, // format = 8 bits per element (byte array)
			uint32(len(imageData)),
			imageData,
		).Check()
		if err != nil {
			return nil, err
		}
		return nil, sendSelectionNotify(conn, event, event.Property)
	}

	// Data too large for direct transfer: signal INCR to caller.
	return imageData, nil
}

// handleTimestampRequest responds to a TIMESTAMP request with the
// ownership acquisition timestamp.
func handleTimestampRequest(
	conn *xgb.Conn,
	event xproto.SelectionRequestEvent,
	timestamp xproto.Timestamp,
) error {
	data := make([]byte, 4)
	xgb.Put32(data, uint32(timestamp))

	err := xproto.ChangePropertyChecked(
		conn,
		xproto.PropModeReplace,
		event.Requestor,
		event.Property,
		xproto.AtomInteger,
		32,
		1,
		data,
	).Check()
	if err != nil {
		return err
	}

	return sendSelectionNotify(conn, event, event.Property)
}

// refuseRequest sends a SelectionNotify with Property=None, indicating
// the request cannot be fulfilled.
func refuseRequest(conn *xgb.Conn, event xproto.SelectionRequestEvent) error {
	return sendSelectionNotify(conn, event, xproto.AtomNone)
}

// sendSelectionNotify sends a SelectionNotify event back to the requestor.
func sendSelectionNotify(
	conn *xgb.Conn,
	event xproto.SelectionRequestEvent,
	property xproto.Atom,
) error {
	notify := xproto.SelectionNotifyEvent{
		Time:      event.Time,
		Requestor: event.Requestor,
		Selection: event.Selection,
		Target:    event.Target,
		Property:  property,
	}

	return xproto.SendEventChecked(
		conn,
		false,
		event.Requestor,
		0, // event mask
		string(notify.Bytes()),
	).Check()
}

// atomsToBytes converts a slice of Atom values to a byte slice
// suitable for ChangeProperty with format=32.
func atomsToBytes(atoms []xproto.Atom) []byte {
	data := make([]byte, len(atoms)*4)
	for i, atom := range atoms {
		xgb.Put32(data[i*4:], uint32(atom))
	}
	return data
}
