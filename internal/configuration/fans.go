package configuration

type FanConfig struct {
	ID        string          `json:"id"`
	NeverStop bool            `json:"neverStop"`
	StartPwm  *int            `json:"startPwm,omitempty"`
	Curve     string          `json:"curve"`
	HwMon     *HwMonFanConfig `json:"hwMon,omitempty"`
	File      *FileFanConfig  `json:"file,omitempty"`
	PwmWriteDelay int         `json:"pwmWriteDelay,omitempty"`
}

type HwMonFanConfig struct {
	Platform  string `json:"platform"`
	Index     int    `json:"index"`
	PwmOutput string
	RpmInput  string
}

type FileFanConfig struct {
	Path string `json:"path"`
}
