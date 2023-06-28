package multiplexer

import (
	"io"
	"sync"
	"time"

	"github.com/jumboframes/armorigo/log"
	"github.com/jumboframes/armorigo/synchub"
	"github.com/singchia/geminio/conn"
	"github.com/singchia/geminio/packet"
	"github.com/singchia/geminio/pkg/id"
	"github.com/singchia/geminio/pkg/iodefine"
	"github.com/singchia/go-timer/v2"
	"github.com/singchia/yafsm"
)

const (
	INIT         = "init"
	SESSION_SENT = "session_sent"
	SESSION_RECV = "session_recv"
	SESSIONED    = "sessioned"
	DISMISS_SENT = "dismiss_sent"
	DISMISS_RECV = "dismiss_recv"
	DISMISS_HALF = "dismiss_half"
	DISMISSED    = "dismissed"
	FINI         = "fini"

	ET_SESSIONSENT = "sessionsent"
	ET_SESSIONRECV = "sessionrecv"
	ET_SESSIONACK  = "sessionrecv"
	ET_ERROR       = "error"
	ET_EOF         = "eof"
	ET_DISMISSSENT = "dismisssent"
	ET_DISMISSRECV = "dismissrecv"
	ET_DISMISSACK  = "dismissack"
	ET_FINI        = "fini"
)

type dialogueOpts struct {
	// packet factory
	pf *packet.PacketFactory
	// logger
	log log.Logger
	// delegate
	dlgt Delegate
	// timer
	tmr        timer.Timer
	tmrOutside bool
	// meta
	meta []byte
}

type dialogue struct {
	// under layer
	cn conn.Conn
	// options
	dialogueOpts
	// session id
	negotiatingID       uint64
	dialogueIDPeersCall bool
	dialogueID          uint64
	// synchub
	shub *synchub.SyncHub

	//sm       *sessionMgr
	fsm      *yafsm.FSM
	onceFini *sync.Once

	// mtx protect follows
	mtx       sync.RWMutex
	sessionOK bool

	// to conn layer
	readInCh, writeOutCh     chan packet.Packet
	readOutCh, writeInCh     chan packet.Packet
	readInSize, writeOutSize int
	readOutSize, writeInSize int
	failedCh                 chan packet.Packet

	closeOnce *sync.Once
	// session control
	writeCh chan packet.Packet
}

type DialogueOption func(*dialogue)

// For the default session which is ready for rolling
func OptionDialogueState(state string) DialogueOption {
	return func(dg *dialogue) {
		dg.fsm.SetState(state)
	}
}

// Set the packet factory for packet generating
func OptionDialoguePacketFactory(pf *packet.PacketFactory) DialogueOption {
	return func(dg *dialogue) {
		dg.pf = pf
	}
}

func OptionDialogueLogger(log log.Logger) DialogueOption {
	return func(dg *dialogue) {
		dg.log = log
	}
}

// Set delegate to know online and offline events
func OptionDialogueDelegate(dlgt Delegate) DialogueOption {
	return func(dg *dialogue) {
		dg.dlgt = dlgt
	}
}

func OptionDialogueTimer(tmr timer.Timer) DialogueOption {
	return func(dg *dialogue) {
		dg.tmr = tmr
		dg.tmrOutside = true
	}
}

// OptionDialogueMeta set the meta info for the session
func OptionDialogueMeta(meta []byte) DialogueOption {
	return func(dg *dialogue) {
		dg.meta = meta
	}
}

func OptionDialogueNegotiatingID(negotiatingID uint64, dialogueIDPeersCall bool) DialogueOption {
	return func(dg *dialogue) {
		dg.negotiatingID = negotiatingID
		dg.dialogueIDPeersCall = dialogueIDPeersCall
	}
}

func NewDialogue(cn conn.Conn, opts ...DialogueOption) (*dialogue, error) {
	dg := &dialogue{
		dialogueOpts: dialogueOpts{
			meta: cn.Meta(),
		},
		dialogueID: packet.SessionIDNull,
		cn:         cn,
		fsm:        yafsm.NewFSM(),
		onceFini:   new(sync.Once),
		sessionOK:  true,
		writeCh:    make(chan packet.Packet, 128),
	}
	// options
	for _, opt := range opts {
		opt(dg)
	}
	// io size
	dg.readInCh = make(chan packet.Packet, 128)
	dg.writeInCh = make(chan packet.Packet, 128)
	dg.readOutCh = make(chan packet.Packet, 128)
	// timer
	if !dg.tmrOutside {
		dg.tmr = timer.NewTimer()
	}
	dg.shub = synchub.NewSyncHub(synchub.OptionTimer(dg.tmr))
	// packet factory
	if dg.pf == nil {
		dg.pf = packet.NewPacketFactory(id.NewIDCounter(id.Even))
	}
	// log
	if dg.log == nil {
		dg.log = log.DefaultLog
	}
	// states
	dg.initFSM()
	// rolling up
	go dg.readPkt()
	go dg.writePkt()
	return dg, nil
}

func (dg *dialogue) Meta() []byte {
	return dg.meta
}

func (dg *dialogue) DialogueID() uint64 {
	return dg.dialogueID
}

func (dg *dialogue) Side() Side {
	return ServerSide
}

func (dg *dialogue) Write(pkt packet.Packet) error {
	dg.mtx.RLock()
	defer dg.mtx.RUnlock()

	if !dg.sessionOK {
		return io.EOF
	}
	dg.writeInCh <- pkt
	return nil
}

func (dg *dialogue) Read() (packet.Packet, error) {
	pkt, ok := <-dg.readOutCh
	if !ok {
		return nil, io.EOF
	}
	return pkt, nil
}

func (dg *dialogue) initFSM() {
	init := dg.fsm.AddState(INIT)
	sessionsent := dg.fsm.AddState(SESSION_SENT)
	sessionrecv := dg.fsm.AddState(SESSION_RECV)
	sessioned := dg.fsm.AddState(SESSIONED)
	dismisssent := dg.fsm.AddState(DISMISS_SENT)
	dismissrecv := dg.fsm.AddState(DISMISS_RECV)
	dismissed := dg.fsm.AddState(DISMISSED)
	fini := dg.fsm.AddState(FINI)
	dg.fsm.SetState(INIT)

	// sender
	dg.fsm.AddEvent(ET_SESSIONSENT, init, sessionsent)
	dg.fsm.AddEvent(ET_SESSIONACK, sessionsent, sessioned)
	dg.fsm.AddEvent(ET_ERROR, sessionsent, dismissed, dg.closeWrapper)
	dg.fsm.AddEvent(ET_EOF, sessionsent, dismissed)

	// receiver
	dg.fsm.AddEvent(ET_SESSIONRECV, init, sessionrecv)
	dg.fsm.AddEvent(ET_SESSIONACK, sessionrecv, sessioned)
	dg.fsm.AddEvent(ET_ERROR, sessionrecv, dismissed, dg.closeWrapper)
	dg.fsm.AddEvent(ET_EOF, sessionrecv, dismissed)

	// both
	dg.fsm.AddEvent(ET_ERROR, sessioned, dismissed, dg.closeWrapper)
	dg.fsm.AddEvent(ET_EOF, sessioned, dismissed)
	dg.fsm.AddEvent(ET_DISMISSSENT, sessioned, dismisssent)
	dg.fsm.AddEvent(ET_DISMISSRECV, sessioned, dismissrecv)
	dg.fsm.AddEvent(ET_DISMISSACK, dismisssent, dismissed)
	dg.fsm.AddEvent(ET_DISMISSACK, dismissrecv, dismissed)

	// fini
	dg.fsm.AddEvent(ET_FINI, init, fini)
	dg.fsm.AddEvent(ET_FINI, sessionsent, fini)
	dg.fsm.AddEvent(ET_FINI, sessioned, fini)
	dg.fsm.AddEvent(ET_FINI, dismisssent, fini)
	dg.fsm.AddEvent(ET_FINI, dismissrecv, fini)
	dg.fsm.AddEvent(ET_FINI, dismissed, fini)
}

func (dg *dialogue) open() error {
	dg.log.Debugf("session is opening, clientId: %d, dialogueID: %d",
		dg.cn.ClientID(), dg.dialogueID)

	var pkt *packet.SessionPacket
	pkt = dg.pf.NewSessionPacket(dg.negotiatingID, dg.dialogueIDPeersCall, dg.meta)

	dg.mtx.RLock()
	if !dg.sessionOK {
		dg.mtx.RUnlock()
		return io.EOF
	}
	dg.writeCh <- pkt
	dg.mtx.RUnlock()

	sync := dg.shub.New(pkt.PacketID, synchub.WithTimeout(30*time.Second))
	event := <-sync.C()
	return event.Error
}

func (dg *dialogue) Close() {
	dg.closeOnce.Do(func() {
		dg.mtx.RLock()
		defer dg.mtx.RUnlock()
		if !dg.sessionOK {
			return
		}

		dg.log.Debugf("session is closing, clientId: %d, dialogueID: %d",
			dg.cn.ClientID(), dg.dialogueID)

		pkt := dg.pf.NewDismissPacket(dg.dialogueID)
		dg.writeCh <- pkt
	})
}

func (dg *dialogue) CloseWait() {
	// send close packet and wait for the end
	dg.closeOnce.Do(func() {
		dg.mtx.RLock()
		if !dg.sessionOK {
			dg.mtx.RUnlock()
			return
		}

		dg.log.Debugf("session is closing, clientId: %d, dialogueID: %d",
			dg.cn.ClientID(), dg.dialogueID)

		pkt := dg.pf.NewDismissPacket(dg.dialogueID)
		dg.writeCh <- pkt
		dg.mtx.RUnlock()
		// the synchub shouldn't be locked
		sync := dg.shub.New(pkt.PacketID, synchub.WithTimeout(30*time.Second))
		event := <-sync.C()
		if event.Error != nil {
			dg.log.Debugf("session close err: %s, clientId: %d, dialogueID: %d",
				event.Error, dg.cn.ClientID(), dg.dialogueID)
			return
		}
		dg.log.Debugf("session closed, clientId: %d, dialogueID: %d",
			dg.cn.ClientID(), dg.dialogueID)
		return
	})
}

func (dg *dialogue) writePkt() {

	for {
		select {
		case pkt, ok := <-dg.writeCh:
			if !ok {
				dg.log.Debugf("write packet EOF, clientId: %d, dialogueID: %d",
					dg.cn.ClientID(), dg.dialogueID)
				return
			}
			ie := dg.handlePkt(pkt, iodefine.OUT)
			switch ie {
			case iodefine.IOSuccess:
				continue
			case iodefine.IOData:
				err := dg.cn.Write(pkt)
				if err != nil {
					dg.log.Debugf("write down err: %s, clientId: %d, dialogueID: %d",
						err, dg.cn.ClientID(), dg.dialogueID)
					goto CLOSED
				}
			case iodefine.IOClosed:
				goto CLOSED

			case iodefine.IOErr:
				dg.fsm.EmitEvent(ET_ERROR)
				dg.log.Errorf("handle packet return err, clientId: %d, dialogueID: %d", dg.cn.ClientID(), dg.dialogueID)
				goto CLOSED
			}
		case pkt, ok := <-dg.writeInCh:
			if !ok {
				dg.log.Infof("write from up EOF, clientId: %d, dialogueID: %d", dg.cn.ClientID(), dg.dialogueID)
				continue
			}
			dg.log.Tracef("to write down, clientId: %d, dialogueID: %d, packetId: %d, packetType: %s",
				dg.cn.ClientID(), dg.dialogueID, pkt.ID(), pkt.Type().String())
			err := dg.cn.Write(pkt)
			if err != nil {
				if err == io.EOF {
					dg.fsm.EmitEvent(ET_EOF)
					dg.log.Infof("write down EOF, clientId: %d, dialogueID: %d, packetId: %d, packetType: %s",
						dg.cn.ClientID(), dg.dialogueID, pkt.ID(), pkt.Type().String())
				} else {
					dg.fsm.EmitEvent(ET_ERROR)
					dg.log.Infof("write down err: %s, clientId: %d, dialogueID: %d, packetId: %d, packetType: %s",
						err, dg.cn.ClientID(), dg.dialogueID, pkt.ID(), pkt.Type().String())

				}
				goto CLOSED
			}
			continue
		}
	}
CLOSED:
	dg.fini()
}

func (dg *dialogue) readPkt() {
	for {
		pkt, ok := <-dg.readInCh
		if !ok {
			dg.log.Debugf("read down EOF, clientId: %d, dialogueID: %d",
				dg.cn.ClientID(), dg.dialogueID)
			return
		}
		dg.log.Tracef("read %s, clientId: %d, dialogueID: %d, packetId: %d",
			pkt.Type().String(), dg.cn.ClientID(), dg.dialogueID, pkt.ID())
		ie := dg.handlePktWrapper(pkt, iodefine.IN)
		switch ie {
		case iodefine.IOSuccess:
			continue
		case iodefine.IOClosed:
			goto CLOSED
		}
	}
CLOSED:
	dg.fini()
}

func (dg *dialogue) handlePktWrapper(pkt packet.Packet, iotype iodefine.IOType) iodefine.IORet {
	ie := dg.handlePkt(pkt, iodefine.IN)
	switch ie {
	case iodefine.IONewActive:
		return iodefine.IOSuccess

	case iodefine.IONewPassive:
		return iodefine.IOSuccess

	case iodefine.IOClosed:
		return iodefine.IOClosed

	case iodefine.IOData:
		dg.mtx.RLock()
		// TODO
		if !dg.sessionOK {
			dg.mtx.RUnlock()
			return iodefine.IOSuccess
		}
		dg.readOutCh <- pkt
		dg.mtx.RUnlock()
		return iodefine.IOSuccess

	case iodefine.IOErr:
		// TODO 在遇到IOErr之后，还有必要发送Close吗，需要区分情况
		dg.fsm.EmitEvent(ET_ERROR)
		return iodefine.IOClosed

	default:
		return iodefine.IOSuccess
	}
}

func (dg *dialogue) handlePkt(pkt packet.Packet, iotype iodefine.IOType) iodefine.IORet {

	switch iotype {
	case iodefine.OUT:
		switch realPkt := pkt.(type) {
		case *packet.SessionPacket:
			err := dg.fsm.EmitEvent(ET_SESSIONSENT)
			if err != nil {
				dg.log.Errorf("emit ET_SESSIONSENT err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), realPkt.NegotiateID, pkt.ID())
				return iodefine.IOErr
			}
			err = dg.cn.Write(realPkt)
			if err != nil {
				dg.log.Errorf("write SESSION err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), realPkt.NegotiateID, pkt.ID())
				return iodefine.IOErr
			}
			dg.log.Debugf("write session down succeed, clientId: %d, dialogueID: %d, packetId: %d",
				dg.cn.ClientID(), realPkt.NegotiateID, pkt.ID())
			return iodefine.IOSuccess

		case *packet.DismissPacket:

			if dg.fsm.InStates(DISMISS_RECV, DISMISSED) {
				// TODO 两边同时Close的场景
				dg.shub.Ack(realPkt.PacketID, nil)
				dg.log.Debugf("already been dismissed, clientId: %d, dialogueID: %d, packetId: %d",
					dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOSuccess
			}

			err := dg.fsm.EmitEvent(ET_DISMISSSENT)
			if err != nil {
				dg.log.Errorf("emit ET_SESSIONSENT err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOErr
			}
			err = dg.cn.Write(realPkt)
			if err != nil {
				dg.log.Errorf("write DISMISS err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOErr
			}
			dg.log.Debugf("write dismiss down succeed, clientId: %d, dialogueID: %d, packetId: %d",
				dg.cn.ClientID(), dg.dialogueID, pkt.ID())
			return iodefine.IOSuccess

		case *packet.DismissAckPacket:
			err := dg.fsm.EmitEvent(ET_DISMISSACK)
			if err != nil {
				dg.log.Errorf("emit ET_DISMISSACK err: %s, clientId: %d, dialogueID: %d, packetId: %d, state: %s",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID(), dg.fsm.State())
				return iodefine.IOErr
			}
			err = dg.cn.Write(pkt)
			if err != nil {
				dg.log.Errorf("write DISMISSACK err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOErr
			}
			dg.log.Debugf("write dismiss ack down succeed, clientId: %d, dialogueID: %d, packetId: %d",
				dg.cn.ClientID(), dg.dialogueID, pkt.ID())
			return iodefine.IOClosed

		default:
			return iodefine.IOData
		}

	case iodefine.IN:
		switch realPkt := pkt.(type) {
		case *packet.SessionPacket:
			dg.log.Debugf("read session packet, clientId: %d, dialogueID: %d, packetId: %d",
				dg.cn.ClientID(), dg.dialogueID, pkt.ID())
			err := dg.fsm.EmitEvent(ET_SESSIONRECV)
			if err != nil {
				dg.log.Debugf("emit ET_SESSIONRECV err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOErr
			}
			//  分配session id
			dialogueID := realPkt.NegotiateID
			if realPkt.SessionIDAcquire() {
				dialogueID = dg.negotiatingID
			}
			dg.dialogueID = dialogueID
			dg.meta = realPkt.SessionData.Meta

			retPkt := dg.pf.NewSessionAckPacket(realPkt.PacketID, dialogueID, nil)
			err = dg.cn.Write(retPkt)
			if err != nil {
				dg.log.Errorf("write SESSIONACK err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOErr
			}

			// TODO 端到端一致性
			err = dg.fsm.EmitEvent(ET_SESSIONACK)
			if err != nil {
				dg.log.Debugf("emit ET_SESSIONACK err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOErr
			}
			dg.log.Debugf("write session ack down succeed, clientId: %d, dialogueID: %d, packetId: %d",
				dg.cn.ClientID(), dg.dialogueID, pkt.ID())
			if dg.dlgt != nil {
				dg.dlgt.DialogueOnline(dg)
			}
			// accept session
			// 被动打开，创建session
			return iodefine.IONewPassive

		case *packet.SessionAckPacket:
			dg.log.Debugf("read session ack packet, clientId: %d, dialogueID: %d, packetId: %d",
				dg.cn.ClientID(), realPkt.SessionID, pkt.ID())
			err := dg.fsm.EmitEvent(ET_SESSIONACK)
			if err != nil {
				dg.log.Debugf("emit ET_SESSIONACK err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOErr
			}
			dg.dialogueID = realPkt.SessionID
			dg.meta = realPkt.SessionData.Meta
			// 主动打开成功，创建session
			dg.shub.Ack(pkt.ID(), nil)
			if dg.dlgt != nil {
				dg.dlgt.DialogueOnline(dg)
			}

			return iodefine.IONewActive

		case *packet.DismissPacket:
			dg.log.Debugf("read dismiss packet, clientId: %d, dialogueID: %d, packetId: %d",
				dg.cn.ClientID(), dg.dialogueID, pkt.ID())

			if dg.fsm.InStates(DISMISS_SENT, DISMISSED) {
				// TODO 两端同时发起Close的场景
				retPkt := dg.pf.NewDismissAckPacket(realPkt.PacketID,
					realPkt.SessionID, nil)
				dg.mtx.RLock()
				if !dg.sessionOK {
					dg.mtx.RUnlock()
					return iodefine.IOErr
				}
				dg.writeCh <- retPkt
				dg.mtx.RUnlock()
				return iodefine.IOSuccess

			}
			err := dg.fsm.EmitEvent(ET_DISMISSRECV)
			if err != nil {
				dg.log.Debugf("emit ET_DISMISSRECV err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOErr
			}

			// return
			retPkt := dg.pf.NewDismissAckPacket(realPkt.PacketID,
				realPkt.SessionID, nil)
			dg.mtx.RLock()
			if !dg.sessionOK {
				dg.mtx.RUnlock()
				return iodefine.IOErr
			}
			dg.writeCh <- retPkt
			dg.mtx.RUnlock()

			return iodefine.IOSuccess

		case *packet.DismissAckPacket:
			dg.log.Debugf("read dismiss ack packet, clientId: %d, dialogueID: %d, packetId: %d",
				dg.cn.ClientID(), dg.dialogueID, pkt.ID())
			err := dg.fsm.EmitEvent(ET_DISMISSACK)
			if err != nil {
				dg.log.Debugf("emit ET_DISMISSACK err: %s, clientId: %d, dialogueID: %d, packetId: %d",
					err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
				return iodefine.IOErr
			}
			dg.shub.Ack(realPkt.PacketID, nil)
			// 主动关闭成功，关闭session
			return iodefine.IOClosed

		default:
			return iodefine.IOData
		}
	}
	return iodefine.IOErr
}

// input packet
func (dg *dialogue) handleInSessionPacket(pkt *packet.SessionPacket) iodefine.IORet {
	dg.log.Debugf("read session packet succeed, clientId: %d, dialogueID: %d, packetId: %d",
		dg.cn.ClientID(), dg.dialogueID, pkt.ID())
	err := dg.fsm.EmitEvent(ET_SESSIONRECV)
	if err != nil {
		dg.log.Debugf("emit ET_SESSIONRECV err: %s, clientId: %d, dialogueID: %d, packetId: %d",
			err, dg.cn.ClientID(), dg.dialogueID, pkt.ID())
		return iodefine.IOErr
	}
}

func (dg *dialogue) close() {}

func (dg *dialogue) closeWrapper(_ *yafsm.Event) {
	dg.Close()
}

func (dg *dialogue) fini() {
	dg.onceFini.Do(func() {
		dg.log.Debugf("session finished, clientId: %d, dialogueID: %d", dg.cn.ClientID(), dg.dialogueID)

		dg.mtx.Lock()
		dg.sessionOK = false
		close(dg.writeCh)

		close(dg.readInCh)
		close(dg.writeInCh)
		close(dg.readOutCh)

		dg.mtx.Unlock()

		dg.fsm.EmitEvent(ET_FINI)
		dg.fsm.Close()
	})
}
