package btconn

import (
	"bytes"
	"context"
	"errors"
	"net"
	"syscall"
	"time"
)

// The MIT License (MIT)
// Copyright (c) 2013 Cenk Alti

// Dial new connection to the address. Does the BitTorrent protocol handshake.
// Handles encryption. May try to connect again if encryption does not match with given setting.
// Returns a net.Conn that is ready for sending/receiving BitTorrent peer protocol messages.
func Dial(
	addr net.Addr,
	deadline time.Time,
	ourExtensions [8]byte,
	ih [20]byte,
	ourID [20]byte) (
	conn net.Conn, cipher CryptoMethod, peerExtensions [8]byte, peerID [20]byte, err error) {
	return DialContext(context.Background(), nil, addr, deadline, ourExtensions, ih, ourID)
}

// DialControl is the signature of net.Dialer.ControlContext. It runs with the
// final resolved address before the connection is attempted; an error aborts
// the dial.
type DialControl func(ctx context.Context, network, address string, c syscall.RawConn) error

// DialContext is Dial that also aborts when ctx ends: the dial is
// context-aware and the connection is closed if ctx is cancelled during the
// handshake, so a blocked read returns promptly. After a successful return
// the caller owns the connection and must watch ctx itself. control, when not
// nil, is installed as the dialer's ControlContext (the egress policy).
func DialContext(
	ctx context.Context,
	control DialControl,
	addr net.Addr,
	deadline time.Time,
	ourExtensions [8]byte,
	ih [20]byte,
	ourID [20]byte) (
	conn net.Conn, cipher CryptoMethod, peerExtensions [8]byte, peerID [20]byte, err error) {
	// First connection - Connecting to peer
	dialer := net.Dialer{Deadline: deadline, ControlContext: control}
	conn, err = dialer.DialContext(ctx, addr.Network(), addr.String())
	if err != nil {
		return
	}
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopWatch()
	defer func(conn net.Conn) {
		if err != nil {
			conn.Close()
		}
	}(conn)

	// Try to use MPTCP - https://www.mptcp.dev/
	dialer.SetMultipathTCP(true)

	// Write first part of BitTorrent handshake to a buffer because we will use it in both encrypted and unencrypted handshake.
	out := bytes.NewBuffer(make([]byte, 0, 68))
	err = writeHandshake(out, ih, ourID, ourExtensions)
	if err != nil {
		return
	}

	// Handshake must be completed in allowed duration.
	if err = conn.SetDeadline(deadline); err != nil {
		return
	}

	sKey := make([]byte, 20)
	copy(sKey, ih[:])

	provide := RC4

	// Try encryption handshake
	encConn := WrapConn(conn)
	cipher, err = encConn.HandshakeOutgoing(sKey, provide, out.Bytes())
	if err != nil {
		return
	} else {
		conn = encConn
	}

	// Read BT handshake
	var ihRead [20]byte
	peerExtensions, ihRead, err = readHandshake1(conn)
	if err != nil {
		return
	}
	if ihRead != ih {
		err = errors.New("invalid infohash")
		return
	}

	peerID, err = readHandshake2(conn)
	if err != nil {
		return
	}
	if peerID == ourID {
		err = errors.New("peerID matches ourID")
		return
	}
	return
}
