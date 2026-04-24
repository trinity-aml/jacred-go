package tracks

type FFProbeModel struct {
	Streams []FFStream `json:"streams"`
}

type FFStream struct {
	Index         int     `json:"index"`
	CodecName     string  `json:"codec_name"`
	CodecLongName string  `json:"codec_long_name"`
	CodecType     string  `json:"codec_type"`
	Width         *int    `json:"width,omitempty"`
	Height        *int    `json:"height,omitempty"`
	CodedWidth    *int    `json:"coded_width,omitempty"`
	CodedHeight   *int    `json:"coded_height,omitempty"`
	SampleFmt     string  `json:"sample_fmt,omitempty"`
	SampleRate    string  `json:"sample_rate,omitempty"`
	Channels      *int    `json:"channels,omitempty"`
	ChannelLayout string  `json:"channel_layout,omitempty"`
	BitRate       string  `json:"bit_rate,omitempty"`
	Tags          *FFTags `json:"tags,omitempty"`
}

type FFTags struct {
	Language string `json:"language,omitempty"`
	BPS      string `json:"BPS,omitempty"`
	Duration string `json:"DURATION,omitempty"`
	Title    string `json:"title,omitempty"`
}
