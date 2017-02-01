package jmail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
)

var debug = debugT(false)

type debugT bool

func (d debugT) Printf(format string, args ...interface{}) {
	if d {
		log.Printf(format, args...)
	}
}

const (
	SUBJ_PREFIX_ISO2022JP_B = "=?iso-2022-jp?b?"
	SUBJ_PREFIX_ISO2022JP_Q = "=?iso-2022-jp?q?"
	SUBJ_PREFIX_UTF8_B      = "=?utf-8?b?"
	SUBJ_PREFIX_UTF8_Q      = "=?utf-8?q?"
	CHARSET_ISO2022JP       = "iso-2022-jp"
	ENC_QUOTED_PRINTABLE    = "quoted-printable"
	ENC_BASE64              = "base64"
	MEDIATYPE_TEXT          = "text/"
	MEDIATYPE_MULTI         = "multipart/"
	MEDIATYPE_MULTI_REL     = "multipart/related"
	MEDIATYPE_MULTI_ALT     = "multipart/alternative"
)

type Message interface {
	DecSubject() string
	DecBody() ([]byte, error)
	GetFrom() ([]*mail.Address, error)
	GetTo() ([]*mail.Address, error)
	GetHeader(string) string
}

// A Jmessage represents a parsed mail message.
type Jmessage struct {
	*mail.Message
}

var AddressParser = mail.AddressParser{
	//ISO-2022-JP, EUC-JPに対応する
	WordDecoder: &mime.WordDecoder{
		CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
			switch charset {
			case "iso-2022-jp":
				return japanese.ISO2022JP.NewDecoder().Reader(input), nil
			case "euc-jp":
				return japanese.EUCJP.NewDecoder().Reader(input), nil
			default:
				return nil, errors.New("WordDecoder.CharsetReader: Unknown Charset")
			}
		},
	},
}

func ReadMessage(r io.Reader) (msg *Jmessage, err error) {
	origmsg, err := mail.ReadMessage(r)

	return &Jmessage{origmsg}, err
}

func (msg Jmessage) DecSubject() string {
	header := msg.Header
	splitsubj := strings.Fields(header.Get("Subject"))
	var bufSubj bytes.Buffer
	for seq, parts := range splitsubj {
		switch {
		case !strings.HasPrefix(parts, "=?"):
			// エンコードなし
			if seq > 0 {
				// 先頭以外はSpaceで区切りなおし
				bufSubj.WriteByte(' ')
			}
			bufSubj.WriteString(parts)

		case len(parts) > len(SUBJ_PREFIX_ISO2022JP_B) && strings.HasPrefix(strings.ToLower(parts[0:len(SUBJ_PREFIX_ISO2022JP_B)]), SUBJ_PREFIX_ISO2022JP_B):
			// iso-2022-jp / base64
			beforeDecode := parts[len(SUBJ_PREFIX_ISO2022JP_B):strings.LastIndex(parts, "?=")]
			afterDecode := base64.NewDecoder(base64.StdEncoding, bytes.NewBufferString(beforeDecode))
			subj_bytes, _ := ioutil.ReadAll(transform.NewReader(afterDecode, japanese.ISO2022JP.NewDecoder()))
			bufSubj.Write(subj_bytes)

		case len(parts) > len(SUBJ_PREFIX_ISO2022JP_Q) && strings.HasPrefix(strings.ToLower(parts[0:len(SUBJ_PREFIX_ISO2022JP_Q)]), SUBJ_PREFIX_ISO2022JP_Q):
			// iso-2022-jp / quoted-printable
			beforeDecode := parts[len(SUBJ_PREFIX_ISO2022JP_Q):strings.LastIndex(parts, "?=")]
			afterDecode := quotedprintable.NewReader(bytes.NewBufferString(beforeDecode))
			subj_bytes, _ := ioutil.ReadAll(transform.NewReader(afterDecode, japanese.ISO2022JP.NewDecoder()))
			bufSubj.Write(subj_bytes)

		case len(parts) > len(SUBJ_PREFIX_UTF8_B) && strings.HasPrefix(strings.ToLower(parts[0:len(SUBJ_PREFIX_UTF8_B)]), SUBJ_PREFIX_UTF8_B):
			// utf-8 / base64
			beforeDecode := parts[len(SUBJ_PREFIX_UTF8_B):strings.LastIndex(parts, "?=")]
			subj_bytes, _ := base64.StdEncoding.DecodeString(beforeDecode)
			bufSubj.Write(subj_bytes)

		case len(parts) > len(SUBJ_PREFIX_UTF8_Q) && strings.HasPrefix(strings.ToLower(parts[0:len(SUBJ_PREFIX_UTF8_Q)]), SUBJ_PREFIX_UTF8_Q):
			// utf-8 / quoted-printable
			beforeDecode := parts[len(SUBJ_PREFIX_UTF8_Q):strings.LastIndex(parts, "?=")]
			afterDecode := quotedprintable.NewReader(bytes.NewBufferString(beforeDecode))
			subj_bytes, _ := ioutil.ReadAll(afterDecode)
			bufSubj.Write(subj_bytes)
		}
	}
	return bufSubj.String()
}

func (msg Jmessage) DecBody() (mailbody []byte, err error) {
	contentType := msg.Header.Get("Content-Type")
	if contentType == "" {
		return readPlainText(map[string][]string(msg.Header), msg.Body)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	debug.Printf("MediaType: %s, %v\n", mediaType, params)
	if err != nil {
		debug.Printf("Error: %v", err)
		return
	}
	mailbody = make([]byte, 0)
	if strings.HasPrefix(mediaType, MEDIATYPE_MULTI) {
		// multipart/...
		mr := multipart.NewReader(msg.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				return mailbody, err
			}
			if err != nil {
				debug.Printf("Error: %v", err)
			}
			mt, _, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
			if err != nil {
				debug.Printf("Error: %v", err)
				return nil, err
			}
			debug.Printf("MediaType-inner: %s\n", mt)
			if strings.HasPrefix(mt, MEDIATYPE_TEXT) {
				// text/plain
				return readPlainText(p.Header, p)
			}
			if strings.HasPrefix(mt, MEDIATYPE_MULTI_ALT) {
				// multipart/alternative
				return readAlternative(p)
			}
			// slurp, err := ioutil.ReadAll(p)
			// if err != nil {
			//   debug.Printf("Error: %v", err)
			// }
			// for key, values := range p.Header {
			//   debug.Printf("%s:%v", key, values)
			// }
			// fmt.Printf("Slurping...: %q\n", slurp)
		}
	} else {
		// text/plain, text/html
		return readPlainText(map[string][]string(msg.Header), msg.Body)
	}
	return
}

// Read body from text/plain
func readPlainText(header textproto.MIMEHeader, body io.Reader) (mailbody []byte, err error) {
	contentType := header.Get("Content-Type")
	encoding := header.Get("Content-Transfer-Encoding")
	_, params, err := mime.ParseMediaType(contentType)
	if encoding == ENC_QUOTED_PRINTABLE {
		if strings.ToLower(params["charset"]) == CHARSET_ISO2022JP {
			mailbody, err = ioutil.ReadAll(transform.NewReader(quotedprintable.NewReader(body), japanese.ISO2022JP.NewDecoder()))
		} else {
			mailbody, err = ioutil.ReadAll(quotedprintable.NewReader(body))
		}
	} else if encoding == ENC_BASE64 {
		mailbody, err = ioutil.ReadAll(base64.NewDecoder(base64.StdEncoding, body))
	} else if len(contentType) == 0 || strings.ToLower(params["charset"]) == CHARSET_ISO2022JP {
		// charset=ISO-2022-JP
		mailbody, err = ioutil.ReadAll(transform.NewReader(body, japanese.ISO2022JP.NewDecoder()))
	} else {
		// encoding = 8bit or 7bit
		mailbody, err = ioutil.ReadAll(body)
	}
	return mailbody, err
}

// Read body from multipart/alternative
func readAlternative(part *multipart.Part) (mailbody []byte, err error) {
	contentType := part.Header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(contentType)
	mr := multipart.NewReader(part, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return mailbody, err
		}
		if err != nil {
			debug.Printf("Error: %v", err)
		}
		mt, _, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if err != nil {
			debug.Printf("Error: %v", err)
			return nil, err
		}
		debug.Printf("MediaType-innerAlt: %s\n", mt)
		if strings.HasPrefix(mt, MEDIATYPE_TEXT) {
			return readPlainText(p.Header, p)
		}
	}
}

func (j *Jmessage) GetFrom() ([]*mail.Address, error) {
	list, err := AddressParser.ParseList(j.Header.Get("From"))
	return list, err
}

func (j *Jmessage) GetTo() ([]*mail.Address, error) {
	list, err := AddressParser.ParseList(j.Header.Get("To"))
	return list, err
}

func (j *Jmessage) GetHeader(key string) string {
	return j.Header.Get(key)
}
