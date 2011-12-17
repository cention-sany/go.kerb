package kerb

import (
	"crypto/rand"
	"encoding/asn1"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

func initSequenceNumber() (ret uint32) {
	if err := binary.Read(rand.Reader, binary.BigEndian, &ret); err != nil {
		panic(err)
	}
	return
}

// To ensure the authenticator is unique we use the microseconds field as a
// sequence number as its required anyways
var usSequenceNumber uint32 = initSequenceNumber()

func nextSequenceNumber() int {
	return int(atomic.AddUint32(&usSequenceNumber, 1))
}

type request struct {
	client  principalName
	crealm  string
	ckey    cipher // only needed for AS requests when tgt == nil
	service principalName
	srealm  string
	till    time.Time
	flags   int
	tgt     *Ticket

	// Setup by request.do()
	nonce  uint32
	time   time.Time
	seqnum int
	sock   net.Conn
	proto  string
}

// send sends a single ticket request down the sock writer. If r.tgt is set
// this is a ticket granting service request, otherwise its an authentication
// service request. Note this does not use any random data, so resending will
// generate the exact same byte stream. This is needed with UDP connections
// such that if the remote receives multiple retries it discards the latters
// as replays.
func (r *request) sendRequest() error {
	body := kdcRequestBody{
		Client:       r.client,
		ServiceRealm: r.srealm,
		Service:      r.service,
		Flags:        flagsToBitString(r.flags),
		Till:         r.till,
		Nonce:        r.nonce,
		Algorithms:   supportedAlgorithms,
	}

	bodyData, err := asn1.Marshal(body)
	if err != nil {
		return err
	}

	reqParam := ""
	req := kdcRequest{
		ProtoVersion: kerberosVersion,
		Body:         asn1.RawValue{FullBytes: bodyData},
		// MsgType and Preauth filled out below
	}

	if r.tgt != nil {
		// For TGS requests we stash an AP_REQ for the ticket granting
		// service (using the krbtgt) as a preauth.
		reqParam = tgsRequestParam
		req.MsgType = tgsRequestType

		auth := authenticator{
			ProtoVersion: kerberosVersion,
			ClientRealm:  r.crealm,
			Client:       r.client,
			Microseconds: r.seqnum % 1000000,
			Time:         r.time,
			Checksum:     r.tgt.key.checksum(bodyData, paTgsRequestChecksumKey),
		}

		authData, err := asn1.MarshalWithParams(auth, authenticatorParam)
		if err != nil {
			return err
		}

		app := appRequest{
			ProtoVersion:  kerberosVersion,
			MsgType:       appRequestType,
			Flags:         flagsToBitString(0),
			Ticket:        asn1.RawValue{FullBytes: r.tgt.ticket},
			Authenticator: r.tgt.key.encrypt(authData, paTgsRequestKey),
		}

		appData, err := asn1.MarshalWithParams(app, appRequestParam)
		if err != nil {
			return err
		}

		req.Preauth = []preauth{{paTgsRequest, appData}}
	} else {
		// For AS requests we add a PA-ENC-TIMESTAMP preauth, even if
		// its always required rather than trying to handle the
		// preauth error return.
		reqParam = asRequestParam
		req.MsgType = asRequestType

		ts, err := asn1.Marshal(encryptedTimestamp{r.time, r.seqnum % 1000000})
		if err != nil {
			return err
		}

		enc, err := asn1.Marshal(r.ckey.encrypt(ts, paEncryptedTimestampKey))
		if err != nil {
			return err
		}

		req.Preauth = []preauth{{paEncryptedTimestamp, enc}}
	}

	data, err := asn1.MarshalWithParams(req, reqParam)
	if err != nil {
		return err
	}

	if r.proto == "tcp" {
		if err := binary.Write(r.sock, binary.BigEndian, uint32(len(data))); err != nil {
			return err
		}
	}

	if r.proto == "udp" && len(data) > maxUdpWrite {
		return io.ErrShortWrite
	}

	if _, err := r.sock.Write(data); err != nil {
		return err
	}

	return nil
}

type RemoteError struct {
	msg *errorMessage
}

func (e RemoteError) ErrorCode() int {
	return e.msg.ErrorCode
}

func (e RemoteError) Error() string {
	return fmt.Sprintf("kerb: remote error %d", e.msg.ErrorCode)
}

func (r *request) recvReply() (*Ticket, error) {
	var data []byte

	switch r.proto {
	case "tcp":
		// TCP streams prepend a 32bit big endian size before each PDU
		var size uint32
		if err := binary.Read(r.sock, binary.BigEndian, &size); err != nil {
			return nil, err
		}

		data = make([]byte, size)

		if _, err := io.ReadFull(r.sock, data); err != nil {
			return nil, err
		}

	case "udp":
		// UDP PDUs are packed in individual frames
		data = make([]byte, 4096)

		n, err := r.sock.Read(data)
		if err != nil {
			return nil, err
		}

		data = data[:n]

	default:
		panic("")
	}

	if len(data) == 0 {
		return nil, ErrParse
	}

	if (data[0] & 0x1F) == errorType {
		errmsg := errorMessage{}
		if _, err := asn1.UnmarshalWithParams(data, &errmsg, errorParam); err != nil {
			return nil, err
		}
		return nil, RemoteError{&errmsg}
	}

	var msgtype, usage int
	var repparam, encparam string
	var key cipher

	if r.tgt != nil {
		repparam = tgsReplyParam
		msgtype = tgsReplyType
		key = r.tgt.key
		usage = tgsReplySessionKey
		encparam = encTgsReplyParam
	} else {
		repparam = asReplyParam
		msgtype = asReplyType
		key = r.ckey
		usage = asReplyClientKey
		encparam = encAsReplyParam
	}

	// Decode reply body

	rep := kdcReply{}
	if _, err := asn1.UnmarshalWithParams(data, &rep, repparam); err != nil {
		return nil, err
	}

	if rep.MsgType != msgtype || rep.ProtoVersion != kerberosVersion || !nameEquals(rep.Client, r.client) || rep.ClientRealm != r.crealm {
		return nil, ErrProtocol
	}

	// Decode encrypted part

	enc := encryptedKdcReply{}
	if edata, err := key.decrypt(rep.Encrypted, usage); err != nil {
		return nil, err
	} else if _, err := asn1.UnmarshalWithParams(edata, &enc, encparam); err != nil {
		return nil, err
	}

	// The returned service may be different from the request. This
	// happens when we get a tgt of the next server to try.
	if enc.Nonce != r.nonce || enc.ServiceRealm != r.srealm {
		return nil, ErrProtocol
	}

	// Decode ticket

	tkt := ticket{}
	if _, err := asn1.UnmarshalWithParams(rep.Ticket.FullBytes, &tkt, ticketParam); err != nil {
		return nil, err
	}

	key, err := loadKey(enc.Key.Algorithm, enc.Key.Key, tkt.KeyVersion)
	if err != nil {
		return nil, err
	}

	// TODO use enc.Flags to mask out flags which the server refused
	return &Ticket{
		client:    r.client,
		crealm:    r.crealm,
		service:   enc.Service,
		srealm:    enc.ServiceRealm,
		ticket:    rep.Ticket.FullBytes,
		till:      enc.Till,
		renewTill: enc.RenewTill,
		flags:     r.flags,
		key:       key,
	}, nil
}

type Ticket struct {
	client    principalName
	crealm    string
	service   principalName
	srealm    string
	ticket    []byte
	till      time.Time
	renewTill time.Time
	flags     int
	key       cipher
	sock      net.Conn
	proto     string
}

func open(proto, realm string) (net.Conn, error) {
	if proto != "tcp" && proto != "udp" {
		panic("invalid protocol: " + proto)
	}

	_, addrs, err := net.LookupSRV("kerberos", proto, realm)

	if err != nil {
		_, addrs, err = net.LookupSRV("kerberos-master", proto, realm)
		if err != nil {
			return nil, err
		}
	}

	var sock net.Conn

	for _, a := range addrs {
		addr := net.JoinHostPort(a.Target, strconv.Itoa(int(a.Port)))
		sock, err = net.Dial(proto, addr)
		if err == nil {
			break
		}
	}

	if err != nil {
		return nil, err
	}

	if proto == "udp" {
		// For datagram connections, we retry up to three times, then give up
		sock.SetReadTimeout(udpReadTimeout)
	}

	return sock, nil
}

type timeoutError interface {
	Timeout() bool
}

func (r *request) do() (tkt *Ticket, err error) {
	r.nonce = 0

	if r.proto == "" {
		r.proto = "udp"
	}

	// Limit the number of retries before we give up and error out with
	// the last error
	for i := 0; i < 3; i++ {
		if r.sock == nil {
			if r.sock, err = open(r.proto, r.srealm); err != nil {
				break
			}
		}

		if r.nonce == 0 {
			// Reduce the entropy of the nonce to 31 bits to ensure it fits in a 4
			// byte asn.1 value. Active directory seems to need this.
			if err = binary.Read(rand.Reader, binary.BigEndian, &r.nonce); err != nil {
				return nil, err
			}
			r.nonce >>= 1
			r.time = time.Now()
			r.seqnum = nextSequenceNumber()
		}

		// TODO what error do we get if the tcp socket has been closed underneath us
		err = r.sendRequest()

		if r.proto == "udp" && err == io.ErrShortWrite {
			r.nonce = 0
			r.proto = "tcp"
			r.sock.Close()
			r.sock = nil
			continue
		} else if err != nil {
			break
		}

		tkt, err = r.recvReply()

		if err == nil {
			return tkt, nil

		} else if e, ok := err.(RemoteError); r.proto == "udp" && ok && e.ErrorCode() == KRB_ERR_RESPONSE_TOO_BIG {
			r.nonce = 0
			r.proto = "tcp"
			r.sock.Close()
			r.sock = nil
			continue

		} else if e, ok := err.(timeoutError); r.proto == "udp" && ok && e.Timeout() {
			// Try again for UDP timeouts.  Reuse nonce, time, and
			// seqnum values so if the multiple requests end up at
			// the server, the server will ignore the retries as
			// replays.
			continue

		} else {
			break
		}
	}

	// Reset the socket if we got some error (even if we could reuse the
	// socket in some cases) so that next time we start with a clean
	// slate.
	r.proto = ""

	if r.sock != nil {
		r.sock.Close()
		r.sock = nil
	}

	return nil, err
}

func (t *Ticket) Principal() string {
	return composePrincipal(t.service)
}

func (t *Ticket) Realm() string {
	return t.srealm
}

func (t *Ticket) ExpiryTime() time.Time {
	return t.till
}
