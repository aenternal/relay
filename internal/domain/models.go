// Package domain defines persistence and desktop command models without UI dependencies.
package domain

const DefaultPreviewBytes = 5 << 20
const DefaultHistoryLimit = 2000

type Workspace struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	ActiveEnvironmentID string `json:"activeEnvironmentId"`
}

type Collection struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId"`
	Name        string    `json:"name"`
	Auth        *HTTPAuth `json:"auth"`
}

type Folder struct {
	ID           string    `json:"id"`
	WorkspaceID  string    `json:"workspaceId"`
	CollectionID string    `json:"collectionId"`
	ParentID     string    `json:"parentId"`
	Name         string    `json:"name"`
	Auth         *HTTPAuth `json:"auth"`
}

type Header struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Enabled bool   `json:"enabled"`
}

type HTTPDefinition struct {
	Method                  string     `json:"method"`
	URL                     string     `json:"url"`
	Version                 string     `json:"version"`
	Headers                 []Header   `json:"headers"`
	Body                    string     `json:"body"`
	PersistBody             bool       `json:"persistBody"`
	Query                   []Header   `json:"query"`
	PathParams              []Header   `json:"pathParams"`
	BodyKind                string     `json:"bodyKind"`
	Form                    []Header   `json:"form"`
	Multipart               []HTTPPart `json:"multipart"`
	FileID                  string     `json:"fileId"`
	Auth                    HTTPAuth   `json:"auth"`
	TLS                     HTTPTLS    `json:"tls"`
	Proxy                   HTTPProxy  `json:"proxy"`
	TimeoutMS               int        `json:"timeoutMs"`
	ConnectTimeoutMS        int        `json:"connectTimeoutMs"`
	TLSHandshakeTimeoutMS   int        `json:"tlsHandshakeTimeoutMs"`
	ResponseHeaderTimeoutMS int        `json:"responseHeaderTimeoutMs"`
	FollowRedirects         bool       `json:"followRedirects"`
	MaxRedirects            int        `json:"maxRedirects"`
	Decompress              bool       `json:"decompress"`
	MaxDownloadBytes        int64      `json:"maxDownloadBytes"`
}

type HTTPPart struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Enabled     bool   `json:"enabled"`
	FileID      string `json:"fileId"`
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
}

type HTTPAuth struct {
	Type     string `json:"type"`
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
	KeyName  string `json:"keyName"`
	KeyValue string `json:"keyValue"`
	In       string `json:"in"`
}

type HTTPTLS struct {
	CAFileID         string `json:"caFileId"`
	ClientCertFileID string `json:"clientCertFileId"`
	ClientKeyFileID  string `json:"clientKeyFileId"`
	ServerName       string `json:"serverName"`
	Insecure         bool   `json:"insecure"`
}

type HTTPProxy struct {
	Mode string `json:"mode"`
	URL  string `json:"url"`
}

type GRPCDefinition struct {
	Endpoint         string   `json:"endpoint"`
	Method           string   `json:"method"`
	SchemaID         string   `json:"schemaId"`
	BodyFormat       string   `json:"bodyFormat"`
	Body             string   `json:"body"`
	PersistBody      bool     `json:"persistBody"`
	Plaintext        bool     `json:"plaintext"`
	TLS              HTTPTLS  `json:"tls"`
	Authority        string   `json:"authority"`
	Metadata         []Header `json:"metadata"`
	TimeoutMS        int      `json:"timeoutMs"`
	ConnectTimeoutMS int      `json:"connectTimeoutMs"`
	MaxSendBytes     int      `json:"maxSendBytes"`
	MaxReceiveBytes  int      `json:"maxReceiveBytes"`
	Compression      string   `json:"compression"`
}

type WebSocketDefinition struct {
	URL              string    `json:"url"`
	Headers          []Header  `json:"headers"`
	Query            []Header  `json:"query"`
	Subprotocols     []string  `json:"subprotocols"`
	TLS              HTTPTLS   `json:"tls"`
	Proxy            HTTPProxy `json:"proxy"`
	ConnectTimeoutMS int       `json:"connectTimeoutMs"`
	WriteTimeoutMS   int       `json:"writeTimeoutMs"`
	ReadTimeoutMS    int       `json:"readTimeoutMs"`
	MaxMessageBytes  int       `json:"maxMessageBytes"`
	BodyKind         string    `json:"bodyKind"`
	BodyEncoding     string    `json:"bodyEncoding"`
	Body             string    `json:"body"`
	PersistBody      bool      `json:"persistBody"`
	FileID           string    `json:"fileId"`
}

type RequestDefinition struct {
	ID           string               `json:"id"`
	WorkspaceID  string               `json:"workspaceId"`
	CollectionID string               `json:"collectionId"`
	FolderID     string               `json:"folderId"`
	Name         string               `json:"name"`
	Protocol     string               `json:"protocol"`
	HTTP         *HTTPDefinition      `json:"http"`
	GRPC         *GRPCDefinition      `json:"grpc"`
	WebSocket    *WebSocketDefinition `json:"websocket"`
}

// A secret variable persists only SecretRef. Values never come back from a vault
// through this model. SecretConfigured is derived from SecretRef, not stored secrets.
type Variable struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Value     string `json:"value"`
	Secret    bool   `json:"secret"`
	SecretRef string `json:"secretRef"`
}

type Environment struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspaceId"`
	Name        string     `json:"name"`
	Variables   []Variable `json:"variables"`
}

type Settings struct {
	Theme                string `json:"theme"`
	ResponsePreviewBytes int    `json:"responsePreviewBytes"`
	HistoryLimit         int    `json:"historyLimit"`
}

func DefaultSettings() Settings {
	return Settings{Theme: "system", ResponsePreviewBytes: DefaultPreviewBytes, HistoryLimit: DefaultHistoryLimit}
}

// Only IDs of saved requests are persisted. Dirty drafts and response bodies are
// deliberately excluded from UIState to keep secrets out of ordinary SQLite.
type UIState struct {
	WorkspaceID     string   `json:"workspaceId"`
	OpenRequestIDs  []string `json:"openRequestIds"`
	ActiveRequestID string   `json:"activeRequestId"`
}

type State struct {
	Revision     int64               `json:"revision"`
	Workspaces   []Workspace         `json:"workspaces"`
	Collections  []Collection        `json:"collections"`
	Folders      []Folder            `json:"folders"`
	Requests     []RequestDefinition `json:"requests"`
	Environments []Environment       `json:"environments"`
	ProtoSchemas []ProtoSchema       `json:"protoSchemas"`
	Settings     Settings            `json:"settings"`
	UI           UIState             `json:"ui"`
}
