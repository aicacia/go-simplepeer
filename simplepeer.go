package simplepeer

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	atomicvalue "github.com/aicacia/go-atomic-value"
	"github.com/aicacia/go-cslice"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

var (
	errInvalidSignalMessageType = fmt.Errorf("invalid signal message type")
	errInvalidSignalMessage     = fmt.Errorf("invalid signal message")
	errInvalidSignalState       = fmt.Errorf("invalid signal state")
	errConnectionNotInitialized = fmt.Errorf("connection not initialized")
)

const (
	SignalMessageRenegotiate        = "renegotiate"
	SignalMessageTransceiverRequest = "transceiverRequest"
	SignalMessageCandidate          = "candidate"
	SignalMessageAnswer             = "answer"
	SignalMessageOffer              = "offer"
	SignalMessagePRAnswer           = "pranswer"
	SignalMessageRollback           = "rollback"
)

type SignalMessageTransceiver struct {
	Kind webrtc.RTPCodecType         `json:"kind"`
	Init []webrtc.RTPTransceiverInit `json:"init"`
}

type OnSignal func(message map[string]interface{}) error
type OnConnect func()
type OnData func(message webrtc.DataChannelMessage)
type OnError func(err error)
type OnClose func()
type OnTransceiver func(transceiver *webrtc.RTPTransceiver)
type OnTrack func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)

type PeerOptions struct {
	Id                    string
	ChannelName           string
	ChannelConfig         *webrtc.DataChannelInit
	InternalChannelConfig *webrtc.DataChannelInit
	Tracks                []webrtc.TrackLocal
	Config                *webrtc.Configuration
	OfferConfig           *webrtc.OfferOptions
	AnswerConfig          *webrtc.AnswerOptions
	OnSignal              OnSignal
	OnConnect             OnConnect
	OnInternalConnect     OnConnect
	OnData                OnData
	OnError               OnError
	OnClose               OnClose
	OnTransceiver         OnTransceiver
	OnTrack               OnTrack
}

type Peer struct {
	id                    string
	initiator             bool
	channelName           string
	channelConfig         *webrtc.DataChannelInit
	channel               *webrtc.DataChannel
	internalChannelConfig *webrtc.DataChannelInit
	internalChannel       *webrtc.DataChannel
	internalChannelReady  *atomic.Bool
	config                webrtc.Configuration
	connection            *webrtc.PeerConnection
	offerConfig           *webrtc.OfferOptions
	answerConfig          *webrtc.AnswerOptions
	onSignal              atomicvalue.AtomicValue[OnSignal]
	onConnect             cslice.CSlice[OnConnect]
	onInternalConnect     cslice.CSlice[OnConnect]
	onData                cslice.CSlice[OnData]
	onError               cslice.CSlice[OnError]
	onClose               cslice.CSlice[OnClose]
	onTransceiver         cslice.CSlice[OnTransceiver]
	onTrack               cslice.CSlice[OnTrack]
	pendingCandidates     cslice.CSlice[webrtc.ICECandidateInit]
}

func NewPeer(options ...PeerOptions) *Peer {
	peer := Peer{
		config: webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{},
		},
		internalChannelReady: &atomic.Bool{},
	}
	for _, option := range options {
		if option.Id != "" {
			peer.id = option.Id
		}
		if option.ChannelName != "" {
			peer.channelName = option.ChannelName
		}
		if option.ChannelConfig != nil {
			peer.channelConfig = option.ChannelConfig
		}
		if option.InternalChannelConfig != nil {
			peer.internalChannelConfig = option.InternalChannelConfig
		}
		if option.Config != nil {
			peer.config = *option.Config
		}
		if option.AnswerConfig != nil {
			peer.answerConfig = option.AnswerConfig
		}
		if option.OfferConfig != nil {
			peer.offerConfig = option.OfferConfig
		}
		if option.OnSignal != nil {
			peer.onSignal.Store(option.OnSignal)
		}
		if option.OnConnect != nil {
			peer.onConnect.Append(option.OnConnect)
		}
		if option.OnInternalConnect != nil {
			peer.onInternalConnect.Append(option.OnInternalConnect)
		}
		if option.OnData != nil {
			peer.onData.Append(option.OnData)
		}
		if option.OnError != nil {
			peer.onError.Append(option.OnError)
		}
		if option.OnClose != nil {
			peer.onClose.Append(option.OnClose)
		}
		if option.OnTransceiver != nil {
			peer.onTransceiver.Append(option.OnTransceiver)
		}
		if option.OnTrack != nil {
			peer.onTrack.Append(option.OnTrack)
		}
	}
	if peer.channelName == "" {
		peer.channelName = uuid.New().String()
	}
	if peer.id == "" {
		peer.id = uuid.New().String()
	}
	return &peer
}

func (peer *Peer) Id() string {
	return peer.id
}

func (peer *Peer) Connection() *webrtc.PeerConnection {
	return peer.connection
}

func (peer *Peer) Channel() *webrtc.DataChannel {
	return peer.channel
}

func (peer *Peer) Initiator() bool {
	return peer.initiator
}

func (peer *Peer) Send(data []byte) error {
	if peer.channel == nil {
		return errConnectionNotInitialized
	}
	return peer.channel.Send(data)
}

func (peer *Peer) Write(data []byte) (n int, err error) {
	err = peer.Send(data)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func (peer *Peer) Read(p []byte) (n int, err error) {
	return 0, errors.New("not implemented")
}

func (peer *Peer) Init() error {
	peer.initiator = true
	err := peer.createPeer()
	if err != nil {
		return err
	}
	return peer.needsNegotiation()
}

func (peer *Peer) AddTransceiverFromKind(kind webrtc.RTPCodecType, init ...webrtc.RTPTransceiverInit) (*webrtc.RTPTransceiver, error) {
	if peer.connection == nil {
		return nil, errConnectionNotInitialized
	}
	if peer.initiator {
		transceiver, err := peer.connection.AddTransceiverFromKind(kind, init...)
		if err != nil {
			return nil, err
		}
		peer.transceiver(transceiver)
		return transceiver, peer.needsNegotiation()
	} else {
		initJSON := make([]map[string]interface{}, 0, len(init))
		for _, transceiverInit := range init {
			initJSON = append(initJSON, map[string]interface{}{
				"direction":     transceiverInit.Direction.String(),
				"sendEncodings": transceiverInit.SendEncodings,
			})
		}
		err := peer.signal(map[string]interface{}{
			"type": SignalMessageTransceiverRequest,
			"transceiverRequest": map[string]interface{}{
				"kind": kind.String(),
				"init": initJSON,
			},
		})
		return nil, err
	}
}

func (peer *Peer) AddTrack(track webrtc.TrackLocal) (*webrtc.RTPSender, error) {
	if peer.connection == nil {
		return nil, errConnectionNotInitialized
	}
	sender, err := peer.connection.AddTrack(track)
	if err != nil {
		return nil, err
	}
	return sender, peer.needsNegotiation()
}

func (peer *Peer) OnSignal(fn OnSignal) {
	peer.onSignal.Store(fn)
}

func (peer *Peer) OnConnect(fn OnConnect) {
	peer.onConnect.Append(fn)
}

func (peer *Peer) OffConnect(fn OnConnect) {
	peer.onConnect.Range(func(index int, onConnect OnConnect) bool {
		if &onConnect == &fn {
			peer.onConnect.Remove(index)
			return false
		}
		return true
	})
}

func (peer *Peer) OnData(fn OnData) {
	peer.onData.Append(fn)
}

func (peer *Peer) OffData(fn OnData) {
	peer.onData.Range(func(index int, onData OnData) bool {
		if &onData == &fn {
			peer.onData.Remove(index)
			return false
		}
		return true
	})
}

func (peer *Peer) OnError(fn OnError) {
	peer.onError.Append(fn)
}

func (peer *Peer) OffError(fn OnError) {
	peer.onError.Range(func(index int, onError OnError) bool {
		if &onError == &fn {
			peer.onError.Remove(index)
			return false
		}
		return true
	})
}

func (peer *Peer) OnClose(fn OnClose) {
	peer.onClose.Append(fn)
}

func (peer *Peer) OffClose(fn OnClose) {
	peer.onClose.Range(func(index int, onClose OnClose) bool {
		if &onClose == &fn {
			peer.onClose.Remove(index)
			return false
		}
		return true
	})
}

func (peer *Peer) OnTransceiver(fn OnTransceiver) {
	peer.onTransceiver.Append(fn)
}

func (peer *Peer) OffTransceiver(fn OnTransceiver) {
	peer.onTransceiver.Range(func(index int, onTransceiver OnTransceiver) bool {
		if &onTransceiver == &fn {
			peer.onTransceiver.Remove(index)
			return false
		}
		return true
	})
}

func (peer *Peer) OnTrack(fn OnTrack) {
	peer.onTrack.Append(fn)
}

func (peer *Peer) OffTrack(fn OnTrack) {
	peer.onTrack.Range(func(index int, onTrack OnTrack) bool {
		if &onTrack == &fn {
			peer.onTrack.Remove(index)
			return false
		}
		return true
	})
}

func (peer *Peer) signal(message map[string]interface{}) error {
	if peer.internalChannelReady.Load() {
		messageBytes, err := json.Marshal(message)
		if err != nil {
			return err
		}
		if err := peer.internalChannel.Send(messageBytes); err != nil {
			return err
		}
		return nil
	} else {
		return peer.onSignal.Load()(message)
	}
}

func (peer *Peer) Signal(message map[string]interface{}) error {
	if peer.connection == nil {
		err := peer.createPeer()
		if err != nil {
			return err
		}
	}
	messageType, ok := message["type"].(string)
	if !ok {
		return errInvalidSignalMessageType
	}
	slog.Debug(fmt.Sprintf("%s: received signal message=%s", peer.id, messageType))
	switch messageType {
	case SignalMessageRenegotiate:
		return peer.needsNegotiation()
	case SignalMessageTransceiverRequest:
		if !peer.initiator {
			return errInvalidSignalState
		}
		transceiverRequestRaw, ok := message["transceiverRequest"].(map[string]interface{})
		if !ok {
			return errInvalidSignalMessage
		}
		var kind webrtc.RTPCodecType
		if kindRaw, ok := transceiverRequestRaw["kind"].(string); ok {
			kind = webrtc.NewRTPCodecType(kindRaw)
		} else {
			return errInvalidSignalMessageType
		}
		var init []webrtc.RTPTransceiverInit
		if initsRaw, ok := transceiverRequestRaw["init"].([]map[string]interface{}); ok {
			for _, initRaw := range initsRaw {
				var direction webrtc.RTPTransceiverDirection
				if directionRaw, ok := initRaw["direction"].(string); ok {
					direction = webrtc.NewRTPTransceiverDirection(directionRaw)
				} else {
					return errInvalidSignalMessage
				}
				sendEncodingsRaw, ok := initRaw["sendEncodings"].([]map[string]interface{})
				if !ok {
					return errInvalidSignalMessage
				}
				sendEncodings := make([]webrtc.RTPEncodingParameters, len(sendEncodingsRaw))
				for i, sendEncodingRaw := range sendEncodingsRaw {
					err := fromJSON[webrtc.RTPEncodingParameters](sendEncodingRaw, &sendEncodings[i])
					if err != nil {
						return err
					}
				}
				init = append(init, webrtc.RTPTransceiverInit{
					Direction:     direction,
					SendEncodings: sendEncodings,
				})
			}
		}
		_, err := peer.AddTransceiverFromKind(kind, init...)
		return err
	case SignalMessageCandidate:
		candidateJSON, ok := message["candidate"].(map[string]interface{})
		if !ok {
			return errInvalidSignalMessage
		}
		var candidate webrtc.ICECandidateInit
		if candidateRaw, ok := candidateJSON["candidate"].(string); ok {
			candidate.Candidate = candidateRaw
		} else {
			return errInvalidSignalMessage
		}
		if sdpMidRaw, ok := candidateJSON["sdpMid"].(string); ok {
			candidate.SDPMid = &sdpMidRaw
		}
		if sdpMLineIndexRaw, ok := candidateJSON["sdpMLineIndex"].(float64); ok {
			sdpMLineIndex := uint16(sdpMLineIndexRaw)
			candidate.SDPMLineIndex = &sdpMLineIndex
		}
		if usernameFragmentRaw, ok := candidateJSON["usernameFragment"].(string); ok {
			candidate.UsernameFragment = &usernameFragmentRaw
		}
		if peer.connection.RemoteDescription() == nil {
			peer.pendingCandidates.Append(candidate)
			return nil
		} else {
			return peer.connection.AddICECandidate(candidate)
		}
	case SignalMessageAnswer:
		fallthrough
	case SignalMessageOffer:
		fallthrough
	case SignalMessagePRAnswer:
		fallthrough
	case SignalMessageRollback:
		sdpRaw, ok := message["sdp"].(string)
		if !ok {
			return errInvalidSignalMessage
		}
		sdp := webrtc.SessionDescription{
			Type: webrtc.NewSDPType(messageType),
			SDP:  sdpRaw,
		}
		slog.Debug(fmt.Sprintf("%s: setting remote sdp", peer.id))
		if err := peer.connection.SetRemoteDescription(sdp); err != nil {
			return err
		}
		var errs []error
		for candidate := range peer.pendingCandidates.Iter() {
			if err := peer.connection.AddICECandidate(candidate); err != nil {
				errs = append(errs, err)
			}
		}
		peer.pendingCandidates.Clear()
		remoteDescription := peer.connection.RemoteDescription()
		if remoteDescription == nil {
			errs = append(errs, webrtc.ErrNoRemoteDescription)
		} else if remoteDescription.Type == webrtc.SDPTypeOffer {
			err := peer.createAnswer()
			if err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	default:
		slog.Debug(fmt.Sprintf("%s: invalid signal type: %+v", peer.id, message))
		return errInvalidSignalMessageType
	}
}

func (peer *Peer) Close() error {
	return peer.close(false)
}

func (peer *Peer) close(triggerCallbacks bool) error {
	var err1, err2 error
	if peer.channel != nil {
		err1 = peer.channel.Close()
		peer.channel = nil
		triggerCallbacks = true
	}
	if peer.connection != nil {
		err2 = peer.connection.Close()
		peer.connection = nil
		triggerCallbacks = true
	}
	if triggerCallbacks {
		for fn := range peer.onClose.Iter() {
			go fn()
		}
	}
	return errors.Join(err1, err2)
}

func (peer *Peer) createPeer() error {
	err := peer.close(false)
	if err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("%s: creating peer", peer.id))
	peer.connection, err = webrtc.NewPeerConnection(peer.config)
	if err != nil {
		return err
	}
	peer.connection.OnConnectionStateChange(peer.onConnectionStateChange)
	peer.connection.OnICECandidate(peer.onICECandidate)
	peer.connection.OnNegotiationNeeded(peer.onNegotiationNeeded)
	peer.connection.OnTrack(peer.onTrackRemote)
	if peer.initiator {
		peer.channel, err = peer.connection.CreateDataChannel(peer.channelName, peer.channelConfig)
		if err != nil {
			return err
		}
		peer.channel.OnError(peer.onDataChannelError)
		peer.channel.OnOpen(peer.onDataChannelOpen)
		peer.channel.OnClose(peer.onDataChannelClose)
		peer.channel.OnMessage(peer.onDataChannelMessage)

		peer.internalChannel, err = peer.connection.CreateDataChannel("internal", peer.internalChannelConfig)
		if err != nil {
			return err
		}
		peer.internalChannel.OnError(peer.onInternalDataChannelError)
		peer.internalChannel.OnOpen(peer.onInternalDataChannelOpen)
		peer.internalChannel.OnOpen(peer.onInternalDataChannelClose)
		peer.internalChannel.OnMessage(peer.onInternalDataChannelMessage)
	} else {
		peer.connection.OnDataChannel(peer.onDataChannel)
	}
	slog.Debug(fmt.Sprintf("%s: created peer", peer.id))
	return nil
}

func (peer *Peer) needsNegotiation() error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	if peer.initiator {
		slog.Debug(fmt.Sprintf("%s: needs negotiation", peer.id))
		return peer.negotiate()
	}
	return nil
}

func (peer *Peer) negotiate() error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	if peer.initiator {
		return peer.createOffer()
	} else {
		return peer.signal(map[string]interface{}{
			"type":        SignalMessageRenegotiate,
			"renegotiate": true,
		})
	}
}

func (peer *Peer) createOffer() error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	slog.Debug(fmt.Sprintf("%s: creating offer", peer.id))
	offer, err := peer.connection.CreateOffer(peer.offerConfig)
	if err != nil {
		return err
	}
	if err := peer.connection.SetLocalDescription(offer); err != nil {
		return err
	}
	offerJSON, err := toJSON(offer)
	if err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("%s: created offer", peer.id))
	return peer.signal(offerJSON)
}

func (peer *Peer) createAnswer() error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	slog.Debug(fmt.Sprintf("%s: creating answer", peer.id))
	answer, err := peer.connection.CreateAnswer(peer.answerConfig)
	if err != nil {
		return err
	}
	if err := peer.connection.SetLocalDescription(answer); err != nil {
		return err
	}
	answerJSON, err := toJSON(answer)
	if err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("%s: created answer", peer.id))
	return peer.signal(answerJSON)
}

func (peer *Peer) connect() {
	for fn := range peer.onConnect.Iter() {
		go fn()
	}
}

func (peer *Peer) internalConnect() {
	for fn := range peer.onInternalConnect.Iter() {
		go fn()
	}
}

func (peer *Peer) error(err error) {
	handled := false
	for fn := range peer.onError.Iter() {
		go fn(err)
		handled = true
	}
	if !handled {
		slog.Error(fmt.Sprintf("%s: unhandled: %s", peer.id, err))
	}
}

func (peer *Peer) transceiver(transceiver *webrtc.RTPTransceiver) {
	for fn := range peer.onTransceiver.Iter() {
		go fn(transceiver)
	}
}

func (peer *Peer) track(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	for fn := range peer.onTrack.Iter() {
		go fn(track, receiver)
	}
}

func (peer *Peer) onDataChannelError(err error) {
	peer.error(err)
}

func (peer *Peer) onDataChannelOpen() {
	peer.connect()
}

func (peer *Peer) onDataChannelClose() {
}

func (peer *Peer) onDataChannelMessage(message webrtc.DataChannelMessage) {
	for fn := range peer.onData.Iter() {
		go fn(message)
	}
}

func (peer *Peer) onInternalDataChannelError(err error) {
	peer.error(err)
}

func (peer *Peer) onInternalDataChannelOpen() {
	peer.internalChannelReady.Store(true)
	peer.internalConnect()
}

func (peer *Peer) onInternalDataChannelClose() {
	peer.internalChannelReady.Store(false)
}

func (peer *Peer) onInternalDataChannelMessage(message webrtc.DataChannelMessage) {
	var msg map[string]interface{}
	if err := json.Unmarshal(message.Data, &msg); err != nil {
		slog.Debug(fmt.Sprintf("%s: invalid internal data channel message: %s", peer.id, message.Data))
		slog.Error(fmt.Sprintf("%s: invalid internal data channel message: %s", peer.id, err))
		return
	}
	if err := peer.Signal(msg); err != nil {
		slog.Error(fmt.Sprintf("%s: internal data channel message error: %s", peer.id, err))
		return
	}
}

func (peer *Peer) onConnectionStateChange(pcs webrtc.PeerConnectionState) {
	switch pcs {
	case webrtc.PeerConnectionStateUnknown:
		slog.Debug(fmt.Sprintf("%s: connection state unknown", peer.id))
	case webrtc.PeerConnectionStateNew:
		slog.Debug(fmt.Sprintf("%s: connection new", peer.id))
	case webrtc.PeerConnectionStateConnecting:
		slog.Debug(fmt.Sprintf("%s: connecting", peer.id))
	case webrtc.PeerConnectionStateConnected:
		slog.Debug(fmt.Sprintf("%s: connection established", peer.id))
	case webrtc.PeerConnectionStateDisconnected:
		slog.Debug(fmt.Sprintf("%s: connection disconnected", peer.id))
		peer.close(true)
	case webrtc.PeerConnectionStateFailed:
		slog.Debug(fmt.Sprintf("%s: connection failed", peer.id))
		peer.close(true)
	case webrtc.PeerConnectionStateClosed:
		slog.Debug(fmt.Sprintf("%s: connection closed", peer.id))
		peer.close(true)
	}
}

func (peer *Peer) onICECandidate(pendingCandidate *webrtc.ICECandidate) {
	if peer.connection == nil || pendingCandidate == nil {
		return
	}
	if peer.connection.RemoteDescription() == nil {
		peer.pendingCandidates.Append(pendingCandidate.ToJSON())
	} else {
		iceCandidateInit := pendingCandidate.ToJSON()
		iceCandidateInitJSON, err := toJSON(iceCandidateInit)
		if err != nil {
			peer.error(err)
			return
		}
		err = peer.signal(map[string]interface{}{
			"type":      SignalMessageCandidate,
			"candidate": iceCandidateInitJSON,
		})
		if err != nil {
			peer.error(err)
		}
	}
}

func (peer *Peer) onNegotiationNeeded() {
}

func (peer *Peer) onTrackRemote(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	peer.track(track, receiver)
}

func (peer *Peer) onDataChannel(channel *webrtc.DataChannel) {
	if channel != nil {
		if channel.Label() == "internal" {
			peer.internalChannel = channel
			peer.internalChannel.OnError(peer.onInternalDataChannelError)
			peer.internalChannel.OnOpen(peer.onInternalDataChannelOpen)
			peer.internalChannel.OnClose(peer.onInternalDataChannelClose)
			peer.internalChannel.OnMessage(peer.onInternalDataChannelMessage)
		} else {
			peer.channel = channel
			peer.channel.OnError(peer.onDataChannelError)
			peer.channel.OnOpen(peer.onDataChannelOpen)
			peer.channel.OnClose(peer.onDataChannelClose)
			peer.channel.OnMessage(peer.onDataChannelMessage)
		}
	}
}

func toJSON(v interface{}) (map[string]interface{}, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func fromJSON[T any](v map[string]interface{}, value *T) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, value); err != nil {
		return err
	}
	return nil
}
