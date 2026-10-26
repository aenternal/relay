package domain

type SelectedFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type HTTPExecution struct {
	ID                string             `json:"id"`
	RequestID         string             `json:"requestId"`
	State             string             `json:"state"`
	StartedAt         string             `json:"startedAt"`
	FinishedAt        string             `json:"finishedAt"`
	URL               string             `json:"url"`
	Protocol          string             `json:"protocol"`
	StatusCode        int                `json:"statusCode"`
	Status            string             `json:"status"`
	Headers           []Header           `json:"headers"`
	Trailers          []Header           `json:"trailers"`
	BodyBase64        string             `json:"bodyBase64"`
	BodyText          string             `json:"bodyText"`
	Binary            bool               `json:"binary"`
	PreviewBytes      int64              `json:"previewBytes"`
	ReceivedBytes     int64              `json:"receivedBytes"`
	Truncated         bool               `json:"truncated"`
	DownloadAvailable bool               `json:"downloadAvailable"`
	Error             string             `json:"error"`
	Warnings          []string           `json:"warnings"`
	Redirects         []HTTPRedirect     `json:"redirects"`
	TLS               *HTTPConnectionTLS `json:"tls"`
	Timing            HTTPTiming         `json:"timing"`
}

type HTTPRedirect struct {
	URL        string `json:"url"`
	StatusCode int    `json:"statusCode"`
}

type HTTPConnectionTLS struct {
	Version            string   `json:"version"`
	ServerName         string   `json:"serverName"`
	NegotiatedProtocol string   `json:"negotiatedProtocol"`
	CipherSuite        string   `json:"cipherSuite"`
	Verified           bool     `json:"verified"`
	Subject            string   `json:"subject"`
	Issuer             string   `json:"issuer"`
	DNSNames           []string `json:"dnsNames"`
	ExpiresAt          string   `json:"expiresAt"`
}

type HTTPTiming struct {
	DNSMS       float64 `json:"dnsMs"`
	ConnectMS   float64 `json:"connectMs"`
	TLSMS       float64 `json:"tlsMs"`
	FirstByteMS float64 `json:"firstByteMs"`
	DownloadMS  float64 `json:"downloadMs"`
	TotalMS     float64 `json:"totalMs"`
}
