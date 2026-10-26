package domain

const DefaultGRPCMessageBytes = 4 << 20

// GRPCExecution contains bounded, sanitized unary/reflection results. Connection
// ownership and cancellation remain in Go; no execution is persisted in SQLite.
type GRPCExecution struct {
	ID                 string             `json:"id"`
	RequestID          string             `json:"requestId"`
	Kind               string             `json:"kind"`
	State              string             `json:"state"`
	StartedAt          string             `json:"startedAt"`
	FinishedAt         string             `json:"finishedAt"`
	Endpoint           string             `json:"endpoint"`
	Method             string             `json:"method"`
	SchemaID           string             `json:"schemaId"`
	Code               int                `json:"code"`
	CodeName           string             `json:"codeName"`
	StatusMessage      string             `json:"statusMessage"`
	Headers            []Header           `json:"headers"`
	Trailers           []Header           `json:"trailers"`
	MessageJSON        string             `json:"messageJSON"`
	WireBase64         string             `json:"wireBase64"`
	WireBytes          int                `json:"wireBytes"`
	Details            []GRPCStatusDetail `json:"details"`
	Error              string             `json:"error"`
	Warnings           []string           `json:"warnings"`
	TLS                *HTTPConnectionTLS `json:"tls"`
	ConnectMS          float64            `json:"connectMs"`
	TotalMS            float64            `json:"totalMs"`
	ReflectionProtocol string             `json:"reflectionProtocol"`
	Truncated          bool               `json:"truncated"`
	RPCType            string             `json:"rpcType"`
	StreamState        string             `json:"streamState"`
	SentCount          int                `json:"sentCount"`
	ReceivedCount      int                `json:"receivedCount"`
}

// GRPCStreamMessage is an ephemeral, sanitized preview of one message. Sequence
// orders both directions; Index counts independently within its direction.
type GRPCStreamMessage struct {
	Sequence    int      `json:"sequence"`
	Direction   string   `json:"direction"`
	Index       int      `json:"index"`
	Timestamp   string   `json:"timestamp"`
	MessageJSON string   `json:"messageJSON"`
	WireBase64  string   `json:"wireBase64"`
	WireBytes   int      `json:"wireBytes"`
	Warnings    []string `json:"warnings"`
	Truncated   bool     `json:"truncated"`
}

type GRPCStreamBatch struct {
	Execution    GRPCExecution       `json:"execution"`
	Messages     []GRPCStreamMessage `json:"messages"`
	NextSequence int                 `json:"nextSequence"`
}

type GRPCStatusDetail struct {
	TypeURL     string `json:"typeURL"`
	MessageJSON string `json:"messageJSON"`
	WireBase64  string `json:"wireBase64"`
	Error       string `json:"error"`
}
