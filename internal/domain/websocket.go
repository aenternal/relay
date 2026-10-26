package domain

// WebSocket executions and event buffers are transient. Saving a bounded session
// is a separate explicit command; payloads are excluded unless requested.
type WebSocketExecution struct {
	ID                  string             `json:"id"`
	RequestID           string             `json:"requestId"`
	WorkspaceID         string             `json:"workspaceId"`
	State               string             `json:"state"`
	URL                 string             `json:"url"`
	StartedAt           string             `json:"startedAt"`
	FinishedAt          string             `json:"finishedAt"`
	HandshakeStatusCode int                `json:"handshakeStatusCode"`
	HandshakeHeaders    []Header           `json:"handshakeHeaders"`
	Subprotocol         string             `json:"subprotocol"`
	CloseCode           int                `json:"closeCode"`
	CloseReason         string             `json:"closeReason"`
	CleanClose          bool               `json:"cleanClose"`
	Error               string             `json:"error"`
	Warnings            []string           `json:"warnings"`
	SentCount           int                `json:"sentCount"`
	ReceivedCount       int                `json:"receivedCount"`
	SentBytes           int64              `json:"sentBytes"`
	ReceivedBytes       int64              `json:"receivedBytes"`
	DroppedEvents       int                `json:"droppedEvents"`
	TLS                 *HTTPConnectionTLS `json:"tls"`
	ConnectMS           float64            `json:"connectMs"`
	TotalMS             float64            `json:"totalMs"`
}

type WebSocketEvent struct {
	Sequence    int      `json:"sequence"`
	Type        string   `json:"type"`
	Direction   string   `json:"direction"`
	MessageType string   `json:"messageType"`
	Timestamp   string   `json:"timestamp"`
	Text        string   `json:"text"`
	DataBase64  string   `json:"dataBase64"`
	Bytes       int      `json:"bytes"`
	Truncated   bool     `json:"truncated"`
	Warnings    []string `json:"warnings"`
}

type WebSocketBatch struct {
	Execution    WebSocketExecution `json:"execution"`
	Events       []WebSocketEvent   `json:"events"`
	NextSequence int                `json:"nextSequence"`
}

type WebSocketSend struct {
	Kind     string `json:"kind"`
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
	FileID   string `json:"fileId"`
}

type WebSocketSavedSession struct {
	ID              string             `json:"id"`
	WorkspaceID     string             `json:"workspaceId"`
	RequestID       string             `json:"requestId"`
	SavedAt         string             `json:"savedAt"`
	IncludePayloads bool               `json:"includePayloads"`
	OmittedEvents   int                `json:"omittedEvents"`
	Execution       WebSocketExecution `json:"execution"`
	Events          []WebSocketEvent   `json:"events"`
}

type WebSocketSessionSummary struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspaceId"`
	RequestID       string `json:"requestId"`
	SavedAt         string `json:"savedAt"`
	URL             string `json:"url"`
	State           string `json:"state"`
	CloseCode       int    `json:"closeCode"`
	EventCount      int    `json:"eventCount"`
	IncludePayloads bool   `json:"includePayloads"`
	OmittedEvents   int    `json:"omittedEvents"`
}
