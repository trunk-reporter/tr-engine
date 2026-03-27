package ingest

import "encoding/json"

// Envelope is the common wrapper for all MQTT messages from trunk-recorder.
type Envelope struct {
	Type       string          `json:"type"`
	Timestamp  int64           `json:"timestamp"`
	InstanceID string          `json:"instance_id"`
	Raw        json.RawMessage `json:"-"` // original payload for archival
}

// CallData represents a call in call_start, call_end, calls_active, and audio messages.
type CallData struct {
	ID                   string  `json:"id"`
	CallNum              int     `json:"call_num"`
	SysNum               int     `json:"sys_num"`
	SysName              string  `json:"sys_name"`
	Freq                 float64 `json:"freq"`
	Unit                 int     `json:"unit"`
	UnitAlphaTag         string  `json:"unit_alpha_tag"`
	Talkgroup            int     `json:"talkgroup"`
	TalkgroupAlphaTag    string  `json:"talkgroup_alpha_tag"`
	TalkgroupDescription string  `json:"talkgroup_description"`
	TalkgroupGroup       string  `json:"talkgroup_group"`
	TalkgroupTag         string  `json:"talkgroup_tag"`
	TalkgroupPatches     string  `json:"talkgroup_patches"`
	Elapsed              int     `json:"elapsed"`
	Length               float64 `json:"length"`
	CallState            int     `json:"call_state"`
	CallStateType        string  `json:"call_state_type"`
	MonState             int     `json:"mon_state"`
	MonStateType         string  `json:"mon_state_type"`
	AudioType            string  `json:"audio_type"`
	Phase2TDMA           bool    `json:"phase2_tdma"`
	TDMASlot             int     `json:"tdma_slot"`
	Analog               bool    `json:"analog"`
	RecNum               int     `json:"rec_num"`
	SrcNum               int     `json:"src_num"`
	RecState             int     `json:"rec_state"`
	RecStateType         string  `json:"rec_state_type"`
	Conventional         bool    `json:"conventional"`
	Encrypted            bool    `json:"encrypted"`
	Emergency            bool    `json:"emergency"`
	StartTime            int64   `json:"start_time"`
	StopTime             int64   `json:"stop_time"`
	// call_end only
	ProcessCallTime float64 `json:"process_call_time"`
	ErrorCount      int     `json:"error_count"`
	SpikeCount      int     `json:"spike_count"`
	RetryAttempt    int     `json:"retry_attempt"`
	FreqError       int     `json:"freq_error"`
	Signal          float64 `json:"signal"`
	Noise           float64 `json:"noise"`
	CallFilename    string  `json:"call_filename"`
	// incident data (optional, from TR plugin)
	IncidentData json.RawMessage `json:"incidentdata,omitempty"`
}

// CallStartMsg wraps a call_start message.
type CallStartMsg struct {
	Envelope
	Call CallData `json:"call"`
}

// CallEndMsg wraps a call_end message.
type CallEndMsg struct {
	Envelope
	Call CallData `json:"call"`
}

// CallsActiveMsg wraps a calls_active message.
type CallsActiveMsg struct {
	Envelope
	Calls []CallData `json:"calls"`
}

// AudioMetadata is the metadata sub-object inside an audio message's call field.
type AudioMetadata struct {
	Freq                float64    `json:"freq"`
	FreqError           int        `json:"freq_error"`
	Signal              float64    `json:"signal"`
	Noise               float64    `json:"noise"`
	SourceNum           int        `json:"source_num"`
	RecorderNum         int        `json:"recorder_num"`
	TDMASlot            int        `json:"tdma_slot"`
	Phase2TDMA          int        `json:"phase2_tdma"`
	StartTime           int64      `json:"start_time"`
	StopTime            int64      `json:"stop_time"`
	Emergency           int        `json:"emergency"`
	Priority            int        `json:"priority"`
	Mode                int        `json:"mode"`
	Duplex              int        `json:"duplex"`
	Encrypted           int        `json:"encrypted"`
	CallLength          int        `json:"call_length"`
	Talkgroup           int        `json:"talkgroup"`
	TalkgroupTag        string     `json:"talkgroup_tag"`
	TalkgroupDesc       string     `json:"talkgroup_description"`
	TalkgroupGroupTag   string     `json:"talkgroup_group_tag"`
	TalkgroupGroup      string     `json:"talkgroup_group"`
	AudioType           string     `json:"audio_type"`
	ShortName           string     `json:"short_name"`
	FreqList            []FreqItem `json:"freqList"`
	SrcList             []SrcItem  `json:"srcList"`
	Filename            string          `json:"filename"`
	Transcript          string          `json:"transcript,omitempty"`        // pre-generated transcription text
	TranscriptWords     json.RawMessage `json:"transcript_words,omitempty"` // optional pre-built word/segment data
	IncidentData        json.RawMessage `json:"incidentdata,omitempty"`
}

// FreqItem is a frequency entry in the audio metadata.
type FreqItem struct {
	Freq       float64 `json:"freq"`
	Time       int64   `json:"time"`
	Pos        float64 `json:"pos"`
	Len        float64 `json:"len"`
	ErrorCount int     `json:"error_count"`
	SpikeCount int     `json:"spike_count"`
}

// SrcItem is a source/transmission entry in the audio metadata.
type SrcItem struct {
	Src          int     `json:"src"`
	Time         int64   `json:"time"`
	Pos          float64 `json:"pos"`
	Emergency    int     `json:"emergency"`
	SignalSystem string  `json:"signal_system"`
	Tag          string  `json:"tag"`
}

// AudioCallData is the "call" field in an audio message.
type AudioCallData struct {
	AudioWavBase64 string        `json:"audio_wav_base64"`
	AudioM4ABase64 string        `json:"audio_m4a_base64"`
	AudioDvcfBase64 string        `json:"audio_dvcf_base64"`
	Metadata       AudioMetadata `json:"metadata"`
}

// AudioMsg wraps an audio message.
type AudioMsg struct {
	Envelope
	Call AudioCallData `json:"call"`
}

// UnitEventData is the inner data for unit event messages (on, off, call, end, join, etc.).
type UnitEventData struct {
	SysNum               int     `json:"sys_num"`
	SysName              string  `json:"sys_name"`
	Unit                 int     `json:"unit"`
	UnitAlphaTag         string  `json:"unit_alpha_tag"`
	Talkgroup            int     `json:"talkgroup"`
	TalkgroupAlphaTag    string  `json:"talkgroup_alpha_tag"`
	TalkgroupDescription string  `json:"talkgroup_description"`
	TalkgroupGroup       string  `json:"talkgroup_group"`
	TalkgroupTag         string  `json:"talkgroup_tag"`
	TalkgroupPatches     string  `json:"talkgroup_patches"`
	CallNum              int     `json:"call_num"`
	Freq                 float64 `json:"freq"`
	Position             float64 `json:"position"`
	Length               float64 `json:"length"`
	Emergency            bool    `json:"emergency"`
	Encrypted            bool    `json:"encrypted"`
	StartTime            int64   `json:"start_time"`
	StopTime             int64   `json:"stop_time"`
	ErrorCount           int     `json:"error_count"`
	SpikeCount           int     `json:"spike_count"`
	SampleCount          int     `json:"sample_count"`
	TransmissionFilename string          `json:"transmission_filename"`
	IncidentData         json.RawMessage `json:"incidentdata,omitempty"`
	// Signal-specific fields (from signal events)
	SignalingType string `json:"signaling_type,omitempty"`
	SignalType    string `json:"signal_type,omitempty"`
}

// UnitEventMsg wraps a unit event message. The event data is keyed by the event type.
type UnitEventMsg struct {
	Envelope
	EventData UnitEventData // extracted from the type-named key
}

// RecorderData represents a single recorder entry.
type RecorderData struct {
	ID           string  `json:"id"`
	SrcNum       int     `json:"src_num"`
	RecNum       int     `json:"rec_num"`
	Type         string  `json:"type"`
	Duration     float64 `json:"duration"`
	Freq         float64 `json:"freq"`
	Count        int     `json:"count"`
	RecState     int     `json:"rec_state"`
	RecStateType string  `json:"rec_state_type"`
	Squelched    bool    `json:"squelched"`
}

// RecordersMsg wraps a recorders (batch) message.
type RecordersMsg struct {
	Envelope
	Recorders []RecorderData `json:"recorders"`
}

// RecorderMsg wraps a single recorder update message.
type RecorderMsg struct {
	Envelope
	Recorder RecorderData `json:"recorder"`
}

// RateData represents a single system's decode rate entry.
type RateData struct {
	SysNum              int     `json:"sys_num"`
	SysName             string  `json:"sys_name"`
	DecodeRate          float64 `json:"decoderate"`
	DecodeRateInterval  float64 `json:"decoderate_interval"`
	ControlChannel      float64 `json:"control_channel"`
}

// RatesMsg wraps a rates message.
type RatesMsg struct {
	Envelope
	Rates []RateData `json:"rates"`
}

// StatusMsg wraps a trunk_recorder/status message.
type StatusMsg struct {
	Envelope
	ClientID string `json:"client_id"`
	Status   string `json:"status"`
}

// ConfigMsg wraps a config message (TR instance configuration snapshot).
type ConfigMsg struct {
	Envelope
	Config ConfigData `json:"config"`
}

// ConfigData is the "config" sub-object containing TR instance settings.
type ConfigData struct {
	CaptureDir   string          `json:"capture_dir"`
	UploadServer string          `json:"upload_server"`
	CallTimeout  float64         `json:"call_timeout"`
	LogFile      json.RawMessage `json:"log_file"` // bool or string in different TR versions
	InstanceID   string          `json:"instance_id"`
	InstanceKey  string          `json:"instance_key"`
}

// SystemInfoData represents a system entry from the systems/system topics.
type SystemInfoData struct {
	SysNum  int    `json:"sys_num"`
	SysName string `json:"sys_name"`
	Type    string `json:"type"`
	Sysid   string `json:"sysid"`
	Wacn    string `json:"wacn"`
	Nac     string `json:"nac"`
	RFSS    int    `json:"rfss"`
	SiteID  int    `json:"site_id"`
}

// SystemsMsg wraps a systems (batch) message.
type SystemsMsg struct {
	Envelope
	Systems []SystemInfoData `json:"systems"`
}

// SystemMsg wraps a single system update message.
type SystemMsg struct {
	Envelope
	System SystemInfoData `json:"system"`
}

// TrunkingMessageData is the inner data for a trunking control channel message.
type TrunkingMessageData struct {
	SysNum       int    `json:"sys_num"`
	SysName      string `json:"sys_name"`
	TrunkMsg     int    `json:"trunk_msg"`
	TrunkMsgType string `json:"trunk_msg_type"`
	Opcode       string `json:"opcode"`
	OpcodeType   string `json:"opcode_type"`
	OpcodeDesc   string `json:"opcode_desc"`
	Meta         string `json:"meta"`
}

// TrunkingMessageMsg wraps a trunking message from trengine/messages/{sys_name}/message.
type TrunkingMessageMsg struct {
	Envelope
	Message TrunkingMessageData `json:"message"`
}

// DvcfMetadata is the metadata sub-object inside a DVCF MQTT message.
// Uses json.RawMessage for SrcList to avoid type mismatches (TR sends
// emergency as bool, but the main SrcItem struct uses int).
type DvcfMetadata struct {
	Talkgroup    int             `json:"talkgroup"`
	TalkgroupTag string          `json:"talkgroup_tag"`
	Freq         float64         `json:"freq"`
	StartTime    int64           `json:"start_time"`
	StopTime     int64           `json:"stop_time"`
	CallLength   int             `json:"call_length"`
	ShortName    string          `json:"short_name"`
	Filename     string          `json:"filename"`
	SrcList      json.RawMessage `json:"srcList"`
}

// DvcfMsg wraps a DVCF message from the mqtt_dvcf plugin.
type DvcfMsg struct {
	AudioDvcfBase64 string       `json:"audio_dvcf_base64"`
	Metadata        DvcfMetadata `json:"metadata"`
}

// ConsoleLogData is the inner data for a trunk-recorder console log message.
type ConsoleLogData struct {
	Time     string `json:"time"`     // ISO 8601 timestamp from TR
	Severity string `json:"severity"` // e.g. "info", "error"
	LogMsg   string `json:"log_msg"`
}

// ConsoleLogMsg wraps a console log from trengine/feeds/trunk_recorder/console.
type ConsoleLogMsg struct {
	Envelope
	Console ConsoleLogData `json:"console"`
}
