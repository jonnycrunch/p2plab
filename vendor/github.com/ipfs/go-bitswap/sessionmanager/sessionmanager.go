package sessionmanager

import (
	"context"
	"sync"
	"time"

	blocks "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	delay "github.com/ipfs/go-ipfs-delay"

	bsbpm "github.com/ipfs/go-bitswap/blockpresencemanager"
	notifications "github.com/ipfs/go-bitswap/notifications"
	bspb "github.com/ipfs/go-bitswap/peerbroker"
	bssession "github.com/ipfs/go-bitswap/session"
	bssim "github.com/ipfs/go-bitswap/sessioninterestmanager"
	exchange "github.com/ipfs/go-ipfs-exchange-interface"
	peer "github.com/libp2p/go-libp2p-core/peer"
)

// Session is a session that is managed by the session manager
type Session interface {
	exchange.Fetcher
	ID() uint64
	ReceiveFrom(peer.ID, []cid.Cid, []cid.Cid, []cid.Cid)
	FilterWanted([]cid.Cid) []cid.Cid
}

type sesTrk struct {
	session Session
	pm      bssession.PeerManager
	srs     bssession.RequestSplitter
}

// SessionFactory generates a new session for the SessionManager to track.
type SessionFactory func(ctx context.Context, id uint64, pm bssession.PeerManager, srs bssession.RequestSplitter, pb *bspb.PeerBroker, bpm *bsbpm.BlockPresenceManager, notif notifications.PubSub, provSearchDelay time.Duration, rebroadcastDelay delay.D) Session

// RequestSplitterFactory generates a new request splitter for a session.
type RequestSplitterFactory func(ctx context.Context) bssession.RequestSplitter

// PeerManagerFactory generates a new peer manager for a session.
type PeerManagerFactory func(ctx context.Context, id uint64) bssession.PeerManager

// SessionManager is responsible for creating, managing, and dispatching to
// sessions.
type SessionManager struct {
	ctx                    context.Context
	sessionFactory         SessionFactory
	sessionInterestManager *bssim.SessionInterestManager
	peerManagerFactory     PeerManagerFactory
	blockPresenceManager   *bsbpm.BlockPresenceManager
	requestSplitterFactory RequestSplitterFactory
	peerBroker             *bspb.PeerBroker
	notif                  notifications.PubSub

	// Sessions
	sessLk   sync.RWMutex
	sessions map[uint64]sesTrk

	// Session Index
	sessIDLk sync.Mutex
	sessID   uint64
}

// New creates a new SessionManager.
func New(ctx context.Context, sessionFactory SessionFactory, sessionInterestManager *bssim.SessionInterestManager, peerManagerFactory PeerManagerFactory,
	blockPresenceManager *bsbpm.BlockPresenceManager, requestSplitterFactory RequestSplitterFactory, peerBroker *bspb.PeerBroker, notif notifications.PubSub) *SessionManager {
	return &SessionManager{
		ctx:                    ctx,
		sessionFactory:         sessionFactory,
		sessionInterestManager: sessionInterestManager,
		peerManagerFactory:     peerManagerFactory,
		blockPresenceManager:   blockPresenceManager,
		requestSplitterFactory: requestSplitterFactory,
		peerBroker:             peerBroker,
		notif:                  notif,
		sessions:               make(map[uint64]sesTrk),
	}
}

// NewSession initializes a session with the given context, and adds to the
// session manager.
func (sm *SessionManager) NewSession(ctx context.Context,
	provSearchDelay time.Duration,
	rebroadcastDelay delay.D) exchange.Fetcher {
	id := sm.GetNextSessionID()
	sessionctx, cancel := context.WithCancel(ctx)

	pm := sm.peerManagerFactory(sessionctx, id)
	srs := sm.requestSplitterFactory(sessionctx)
	session := sm.sessionFactory(sessionctx, id, pm, srs, sm.peerBroker, sm.blockPresenceManager, sm.notif, provSearchDelay, rebroadcastDelay)
	tracked := sesTrk{session, pm, srs}
	sm.sessLk.Lock()
	sm.sessions[id] = tracked
	sm.sessLk.Unlock()
	go func() {
		defer cancel()
		select {
		case <-sm.ctx.Done():
			sm.removeSession(tracked)
		case <-ctx.Done():
			sm.removeSession(tracked)
		}
	}()

	return session
}

func (sm *SessionManager) removeSession(st sesTrk) {
	sm.sessLk.Lock()
	defer sm.sessLk.Unlock()

	delete(sm.sessions, st.session.ID())
}

// GetNextSessionID returns the next sequential identifier for a session.
func (sm *SessionManager) GetNextSessionID() uint64 {
	sm.sessIDLk.Lock()
	defer sm.sessIDLk.Unlock()

	sm.sessID++
	return sm.sessID
}

// ReceiveFrom receives block CIDs from a peer and dispatches to sessions.
// func (sm *SessionManager) ReceiveFrom(from peer.ID, ks []cid.Cid, haves []cid.Cid, dontHaves []cid.Cid) {
// 	sm.sessLk.Lock()
// 	defer sm.sessLk.Unlock()

// 	for _, s := range sm.sessions {
// 		s.session.ReceiveFrom(from, ks, haves, dontHaves)
// 	}
// }

// // IsWanted indicates whether any of the sessions are waiting to receive
// // the block with the given CID.
// func (sm *SessionManager) IsWanted(cid cid.Cid) bool {
// 	sm.sessLk.RLock()
// 	defer sm.sessLk.RUnlock()

// 	for _, s := range sm.sessions {
// 		if s.session.IsWanted(cid) {
// 			return true
// 		}
// 	}
// 	return false
// }

func (sm *SessionManager) ReceiveFrom(p peer.ID, blks []cid.Cid, haves []cid.Cid, dontHaves []cid.Cid) []Session {
	sessions := make([]Session, 0)

	sm.sessLk.Lock()
	defer sm.sessLk.Unlock()

	// Notify each session that is interested in the blocks / HAVEs / DONT_HAVEs
	for _, id := range sm.sessionInterestManager.InterestedSessions(blks, haves, dontHaves) {
		if sess, ok := sm.sessions[id]; ok {
			sess.session.ReceiveFrom(p, blks, haves, dontHaves)
			sessions = append(sessions, sess.session)
		}
	}

	return sessions
}

func (sm *SessionManager) FilterWanted(blks []blocks.Block) ([]blocks.Block, []blocks.Block) {
	// Ask each session that is interested in the blocks if they are expecting
	// to receive the blocks
	ks := make([]cid.Cid, 0, len(blks))
	for _, b := range blks {
		ks = append(ks, b.Cid())
	}

	wanted := cid.NewSet()

	sm.sessLk.Lock()
	defer sm.sessLk.Unlock()

	for _, id := range sm.sessionInterestManager.InterestedSessions(ks, []cid.Cid{}, []cid.Cid{}) {
		if sess, ok := sm.sessions[id]; ok {
			ws := sess.session.FilterWanted(ks)
			for _, c := range ws {
				wanted.Add(c)
			}
		}
	}

	wantedBlks := make([]blocks.Block, 0, wanted.Len())
	notWantedBlks := make([]blocks.Block, 0, len(blks)-wanted.Len())
	for _, b := range blks {
		if wanted.Has(b.Cid()) {
			wantedBlks = append(wantedBlks, b)
		} else {
			notWantedBlks = append(notWantedBlks, b)
		}
	}

	return wantedBlks, notWantedBlks
}