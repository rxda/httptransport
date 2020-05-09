package httpx

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/textproto"
	"net/url"

	"github.com/go-courier/courier"
	"github.com/go-courier/reflectx/typesutil"
	"github.com/go-courier/statuserror"
)

type ResponseWrapper func(v interface{}) *Response

func Compose(responseWrappers ...ResponseWrapper) ResponseWrapper {
	return func(v interface{}) *Response {
		r := ResponseFrom(v)
		for i := len(responseWrappers) - 1; i >= 0; i-- {
			r = responseWrappers[i](r)
		}
		return r
	}
}

func WithStatusCode(statusCode int) ResponseWrapper {
	return func(v interface{}) *Response {
		resp := ResponseFrom(v)
		resp.StatusCode = statusCode
		return resp
	}
}

func WithCookies(cookies ...*http.Cookie) ResponseWrapper {
	return func(v interface{}) *Response {
		resp := ResponseFrom(v)
		resp.Cookies = cookies
		return resp
	}
}

func WithSchema(s interface{}) ResponseWrapper {
	return func(v interface{}) *Response {
		resp := ResponseFrom(v)
		return resp
	}
}

func WithContentType(contentType string) ResponseWrapper {
	return func(v interface{}) *Response {
		resp := ResponseFrom(v)
		resp.ContentType = contentType
		return resp
	}
}

func Metadata(key string, values ...string) courier.Metadata {
	return courier.Metadata{
		key: values,
	}
}

func WithMetadata(metadatas ...courier.Metadata) ResponseWrapper {
	return func(v interface{}) *Response {
		resp := ResponseFrom(v)
		resp.Metadata = courier.FromMetas(metadatas...)
		return resp
	}
}

func ResponseFrom(v interface{}) *Response {
	if r, ok := v.(*Response); ok {
		return r
	}

	response := &Response{}

	if redirectDescriber, ok := v.(RedirectDescriber); ok {
		response.Location = redirectDescriber.Location()
		response.StatusCode = redirectDescriber.StatusCode()
		return response
	}

	if e, ok := v.(error); ok {
		if e != nil {
			statusErr, ok := statuserror.IsStatusErr(e)
			if !ok {
				if e == context.Canceled {
					// https://httpstatuses.com/499
					statusErr = statuserror.NewStatusErr("ContextCanceled", 499*1e6, e.Error())
				} else {
					statusErr = statuserror.NewUnknownErr().WithDesc(e.Error())
				}
			}
			v = statusErr
		}
	}

	response.Value = v

	if metadataCarrier, ok := v.(courier.MetadataCarrier); ok {
		response.Metadata = metadataCarrier.Meta()
	}

	if cookiesDescriber, ok := v.(CookiesDescriber); ok {
		response.Cookies = cookiesDescriber.Cookies()
	}

	if contentTypeDescriber, ok := v.(ContentTypeDescriber); ok {
		response.ContentType = contentTypeDescriber.ContentType()
	}

	if statusDescriber, ok := v.(StatusCodeDescriber); ok {
		response.StatusCode = statusDescriber.StatusCode()
	}

	return response
}

type Upgrader interface {
	Upgrade(w http.ResponseWriter, r *http.Request) error
}

type Response struct {
	// value of Body
	Value       interface{}      `json:"-"`
	Metadata    courier.Metadata `json:"-"`
	Cookies     []*http.Cookie   `json:"-"`
	Location    *url.URL         `json:"-"`
	ContentType string           `json:"-"`
	StatusCode  int              `json:"-"`
}

func (response *Response) Unwrap() error {
	if err, ok := response.Value.(error); ok {
		return err
	}
	return nil
}

func (response *Response) Error() string {
	if err, ok := response.Value.(error); ok {
		return err.Error()
	}
	return "response error"
}

type Transformer interface {
	// name or alias of transformer
	// prefer using some keyword about content-type
	Names() []string
	// create transformer new transformer instance by type
	// in this step will to check transformer is valid for type
	New(context.Context, typesutil.Type) (Transformer, error)

	// named by tag
	NamedByTag() string

	// encode to writer
	EncodeToWriter(w io.Writer, v interface{}) (mediaType string, err error)
	// decode from reader
	DecodeFromReader(r io.Reader, v interface{}, headers ...textproto.MIMEHeader) error

	// Content-Type
	String() string
}

type Encode func(w io.Writer, v interface{}) error

func (response *Response) WriteTo(rw http.ResponseWriter, r *http.Request, resolveEncode func(response *Response) (string, Encode, error)) error {
	defer func() {
		response.Value = nil
	}()

	if upgrader, ok := response.Value.(Upgrader); ok {
		return upgrader.Upgrade(rw, r)
	}

	if response.StatusCode == 0 {
		if response.Value == nil {
			response.StatusCode = http.StatusNoContent
		} else {
			if r.Method == http.MethodPost {
				response.StatusCode = http.StatusCreated
			} else {
				response.StatusCode = http.StatusOK
			}
		}
	}

	if response.Metadata != nil {
		header := rw.Header()
		for key, values := range response.Metadata {
			header[textproto.CanonicalMIMEHeaderKey(key)] = values
		}
	}

	if response.Cookies != nil {
		for i := range response.Cookies {
			cookie := response.Cookies[i]
			if cookie != nil {
				http.SetCookie(rw, cookie)
			}
		}
	}

	if response.Location != nil {
		http.Redirect(rw, r, response.Location.String(), response.StatusCode)
		return nil
	}

	if response.StatusCode == http.StatusNoContent {
		rw.WriteHeader(response.StatusCode)
		return nil
	}

	if response.ContentType != "" {
		rw.Header().Set(HeaderContentType, response.ContentType)
	}

	switch v := response.Value.(type) {
	case courier.Result:
		rw.WriteHeader(response.StatusCode)

		if _, err := v.Into(rw); err != nil {
			return err
		}
	case io.Reader:
		rw.WriteHeader(response.StatusCode)
		if _, err := io.Copy(rw, v); err != nil {
			return err
		}
		if c, ok := v.(io.Closer); ok {
			c.Close()
		}
	default:
		contentType, encode, err := resolveEncode(response)
		if err != nil {
			return err
		}

		if response.ContentType == "" {
			rw.Header().Set(HeaderContentType, mime.FormatMediaType(contentType, map[string]string{
				"charset": "utf-8",
			}))
		}

		rw.WriteHeader(response.StatusCode)

		if err := encode(rw, response.Value); err != nil {
			return err
		}
	}

	return nil
}
