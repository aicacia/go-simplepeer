package simplepeer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

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

const defaultMaxChannelMessageSize = 16384

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
	Tracks                []webrtc.TrackLocal
	Config                *webrtc.Configuration
	OfferConfig           *webrtc.OfferOptions
	AnswerConfig          *webrtc.AnswerOptions
	MaxChannelMessageSize int
	OnSignal              OnSignal
	OnConnect             OnConnect
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
	config                webrtc.Configuration
	connection            *webrtc.PeerConnection
	offerConfig           *webrtc.OfferOptions
	answerConfig          *webrtc.AnswerOptions
	maxChannelMessageSize int
	pendingCandidates     cslice.CSlice[webrtc.ICECandidateInit]
	onSignal              atomicvalue.AtomicValue[OnSignal]
	onConnect             cslice.CSlice[OnConnect]
	onData                cslice.CSlice[OnData]
	onError               cslice.CSlice[OnError]
	onClose               cslice.CSlice[OnClose]
	onTransceiver         cslice.CSlice[OnTransceiver]
	onTrack               cslice.CSlice[OnTrack]
}

func NewPeer(options ...PeerOptions) *Peer {
	peer := Peer{
		config: webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{},
		},
		maxChannelMessageSize: defaultMaxChannelMessageSize,
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
		if option.Config != nil {
			peer.config = *option.Config
		}
		if option.AnswerConfig != nil {
			peer.answerConfig = option.AnswerConfig
		}
		if option.OfferConfig != nil {
			peer.offerConfig = option.OfferConfig
		}
		if option.MaxChannelMessageSize > 0 {
			peer.maxChannelMessageSize = option.MaxChannelMessageSize
		}
		if option.OnSignal != nil {
			peer.onSignal.Store(option.OnSignal)
		}
		if option.OnConnect != nil {
			peer.onConnect.Append(option.OnConnect)
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

func (peer *Peer) WriteText(text string) (int, error) {
	sent := 0
	if peer.channel == nil {
		return sent, errConnectionNotInitialized
	}
	if bytesLeft := len(text); bytesLeft > 0 {
		for bytesLeft > 0 {
			count := bytesLeft
			if count > peer.maxChannelMessageSize {
				count = peer.maxChannelMessageSize
			}
			if err := peer.channel.SendText(text[sent:(sent + count)]); err != nil {
				return sent, err
			}
			bytesLeft -= count
			sent += count
		}
	}
	return sent, nil
}

func (peer *Peer) Write(bytes []byte) (int, error) {
	sent := 0
	if peer.channel == nil {
		return sent, errConnectionNotInitialized
	}
	if bytesLeft := len(bytes); bytesLeft > 0 {
		for bytesLeft > 0 {
			count := bytesLeft
			if count > peer.maxChannelMessageSize {
				count = peer.maxChannelMessageSize
			}
			if err := peer.channel.Send(bytes[sent:(sent + count)]); err != nil {
				return sent, err
			}
			bytesLeft -= count
			sent += count
		}
	}
	return sent, nil
}

func (peer *Peer) Reader() io.ReadCloser {
	pipeReader, pipeWriter := io.Pipe()
	onData := func(message webrtc.DataChannelMessage) {
		pipeWriter.Write(message.Data)
	}
	onClose := func() {
		peer.OffData(onData)
		if err := pipeWriter.Close(); err != nil {
			peer.error(err)
		}
	}
	peer.OnData(onData)

	return &peerReader{
		peer:       peer,
		pipeReader: pipeReader,
		onClose:    onClose,
	}
}
func (peer *Peer) Init() error {
	peer.initiator = true
	if err := peer.createPeer(); err != nil {
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

func (peer *Peer) RemoveTrack(sender *webrtc.RTPSender) error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	if err := peer.connection.RemoveTrack(sender); err != nil {
		return err
	}
	return peer.needsNegotiation()
}

func (peer *Peer) OnSignal(fn OnSignal) {
	peer.onSignal.Store(fn)
}

func (peer *Peer) OnConnect(fn OnConnect) {
	peer.onConnect.Append(fn)
}

func (peer *Peer) OffConnect(fn OnConnect) {
	peer.onConnect.Delete(func(index int, onConnect OnConnect) bool {
		return &onConnect == &fn
	})
}

func (peer *Peer) OnData(fn OnData) {
	peer.onData.Append(fn)
}

func (peer *Peer) OffData(fn OnData) {
	peer.onData.Delete(func(index int, onData OnData) bool {
		return &onData == &fn
	})
}

func (peer *Peer) OnError(fn OnError) {
	peer.onError.Append(fn)
}

func (peer *Peer) OffError(fn OnError) {
	peer.onError.Delete(func(index int, onError OnError) bool {
		return &onError == &fn
	})
}

func (peer *Peer) OnClose(fn OnClose) {
	peer.onClose.Append(fn)
}

func (peer *Peer) OffClose(fn OnClose) {
	peer.onClose.Delete(func(index int, onClose OnClose) bool {
		return &onClose == &fn
	})
}

func (peer *Peer) OnTransceiver(fn OnTransceiver) {
	peer.onTransceiver.Append(fn)
}

func (peer *Peer) OffTransceiver(fn OnTransceiver) {
	peer.onTransceiver.Delete(func(index int, onTransceiver OnTransceiver) bool {
		return &onTransceiver == &fn
	})
}

func (peer *Peer) OnTrack(fn OnTrack) {
	peer.onTrack.Append(fn)
}

func (peer *Peer) OffTrack(fn OnTrack) {
	peer.onTrack.Delete(func(index int, onTrack OnTrack) bool {
		return &onTrack == &fn
	})
}

func (peer *Peer) signal(message map[string]interface{}) error {
	return peer.onSignal.Load()(message)
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
	var channelErr, internalChannelErr, connectionErr error
	if peer.channel != nil {
		channelErr = peer.channel.Close()
		peer.channel = nil
	}
	if peer.connection != nil {
		connectionErr = peer.connection.Close()
		peer.connection = nil
	}
	if triggerCallbacks {
		for fn := range peer.onClose.Iter() {
			go fn()
		}
	}
	return errors.Join(channelErr, internalChannelErr, connectionErr)
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
	peer.connection.OnTrack(peer.onTrackRemote)
	if peer.initiator {
		peer.channel, err = peer.connection.CreateDataChannel(peer.channelName, peer.channelConfig)
		if err != nil {
			return err
		}
		peer.channel.OnError(peer.onDataChannelError)
		peer.channel.OnOpen(peer.onDataChannelOpen)
		peer.channel.OnMessage(peer.onDataChannelMessage)
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
	return peer.negotiate()
}

func (peer *Peer) negotiate() error {
	if peer.connection == nil {
		return errConnectionNotInitialized
	}
	slog.Debug(fmt.Sprintf("%s: needs negotiation", peer.id))
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

func (peer *Peer) onDataChannelMessage(message webrtc.DataChannelMessage) {
	for fn := range peer.onData.Iter() {
		go fn(message)
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

func (peer *Peer) onTrackRemote(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	peer.track(track, receiver)
}

func (peer *Peer) onDataChannel(channel *webrtc.DataChannel) {
	if channel != nil {
		peer.channel = channel
		peer.channel.OnError(peer.onDataChannelError)
		peer.channel.OnOpen(peer.onDataChannelOpen)
		peer.channel.OnMessage(peer.onDataChannelMessage)
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

type peerReader struct {
	closed     bool
	peer       *Peer
	pipeReader *io.PipeReader
	onClose    func()
}

func (reader *peerReader) Read(bytes []byte) (int, error) {
	if reader.closed {
		return 0, io.EOF
	}
	return reader.pipeReader.Read(bytes)
}

func (reader *peerReader) Close() error {
	if reader.closed {
		return io.EOF
	}
	reader.onClose()
	reader.closed = true
	return reader.pipeReader.Close()
}
