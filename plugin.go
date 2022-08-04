package newrelic

import (
	"bytes"
	"net/http"
	"sync"

	"github.com/newrelic/go-agent/v3/newrelic"
	"github.com/roadrunner-server/api/v2/plugins/config"
	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/sdk/v2/utils"
)

const (
	pluginName                string = "new_relic"
	path                      string = "http.new_relic"
	rrNewRelicKey             string = "Rr_newrelic"
	rrNewRelicErr             string = "Rr_newrelic_error"
	newRelicTransactionKey    string = "transaction_name"
	newRelicIgnoreTransaction string = "Rr_newrelic_ignore"
	trueStr                   string = "true"
)

type Plugin struct {
	cfg *Config
	app *newrelic.Application

	writersPool sync.Pool
}

func (p *Plugin) Init(cfg config.Configurer) error {
	const op = errors.Op("new_relic_mdw_init")
	if !cfg.Has(path) {
		return errors.E(op, errors.Disabled)
	}

	err := cfg.UnmarshalKey(path, &p.cfg)
	if err != nil {
		return err
	}

	err = p.cfg.InitDefaults()
	if err != nil {
		return err
	}

	app, err := newrelic.NewApplication(
		newrelic.ConfigAppName(p.cfg.AppName),
		newrelic.ConfigLicense(p.cfg.LicenseKey),
		newrelic.ConfigDistributedTracerEnabled(true),
	)

	if err != nil {
		return err
	}

	p.writersPool = sync.Pool{
		New: func() any {
			wr := new(writer)
			wr.code = -1
			wr.data = nil
			wr.hdrToSend = make(map[string][]string, 10)
			return wr
		},
	}
	p.app = app

	return nil
}

func (p *Plugin) Middleware(next http.Handler) http.Handler { //nolint:gocognit,gocyclo
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		txn := p.app.StartTransaction(r.RequestURI)
		defer txn.End()

		w = txn.SetWebResponse(w)
		txn.SetWebRequestHTTP(r)

		// overwrite original rw, because we need to delete sensitive rr_newrelic headers
		rrWriter := p.getWriter()
		r = newrelic.RequestWithTransactionContext(r, txn)
		next.ServeHTTP(rrWriter, r)

		defer func() {
			w.WriteHeader(rrWriter.code)
			_, _ = w.Write(rrWriter.data)
			p.putWriter(rrWriter)
		}()

		// ignore transaction if the rr_newrelic_ignore key exists and equal to true
		if val := rrWriter.hdrToSend[newRelicIgnoreTransaction]; len(val) > 0 && val[0] == trueStr {
			delete(rrWriter.hdrToSend, rrNewRelicKey)
			delete(rrWriter.hdrToSend, rrNewRelicErr)
			delete(rrWriter.hdrToSend, newRelicIgnoreTransaction)

			for k := range rrWriter.hdrToSend {
				for kk := range rrWriter.hdrToSend[k] {
					w.Header().Add(k, rrWriter.hdrToSend[k][kk])
				}
			}

			txn.Ignore()
			return
		}

		// check for error error
		if len(rrWriter.hdrToSend[rrNewRelicErr]) > 0 {
			err := handleErr(rrWriter.hdrToSend[rrNewRelicErr])
			txn.NoticeError(err)

			// to be sure
			delete(rrWriter.hdrToSend, rrNewRelicKey)
			delete(rrWriter.hdrToSend, rrNewRelicErr)
			delete(rrWriter.hdrToSend, newRelicIgnoreTransaction)

			for k := range rrWriter.hdrToSend {
				for kk := range rrWriter.hdrToSend[k] {
					w.Header().Add(k, rrWriter.hdrToSend[k][kk])
				}
			}

			return
		}

		// no errors, general case
		hdr := rrWriter.hdrToSend[rrNewRelicKey]
		if len(hdr) == 0 {
			// to be sure
			delete(rrWriter.hdrToSend, rrNewRelicKey)
			delete(rrWriter.hdrToSend, newRelicIgnoreTransaction)

			for k := range rrWriter.hdrToSend {
				for kk := range rrWriter.hdrToSend[k] {
					w.Header().Add(k, rrWriter.hdrToSend[k][kk])
				}
			}

			return
		}

		for i := 0; i < len(hdr); i++ {
			key, value := split(utils.AsBytes(hdr[i]))

			if key == nil || value == nil {
				continue
			}

			if bytes.Equal(key, utils.AsBytes(newRelicTransactionKey)) {
				txn.SetName(utils.AsString(value))
				continue
			}

			txn.AddAttribute(utils.AsString(key), utils.AsString(value))
		}

		// delete sensitive information
		delete(rrWriter.hdrToSend, rrNewRelicKey)
		delete(rrWriter.hdrToSend, newRelicIgnoreTransaction)

		// send original data
		for k := range rrWriter.hdrToSend {
			for kk := range rrWriter.hdrToSend[k] {
				w.Header().Add(k, rrWriter.hdrToSend[k][kk])
			}

			delete(rrWriter.hdrToSend, k)
		}
	})
}

func (p *Plugin) Name() string {
	return pluginName
}

func (p *Plugin) getWriter() *writer {
	wr := p.writersPool.Get().(*writer)
	return wr
}

func (p *Plugin) putWriter(w *writer) {
	w.code = -1
	w.data = nil

	for k := range w.hdrToSend {
		delete(w.hdrToSend, k)
	}

	p.writersPool.Put(w)
}
