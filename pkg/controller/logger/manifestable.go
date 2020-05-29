package logger

import (
	mfc "github.com/manifestival/controller-runtime-client"
	mf "github.com/manifestival/manifestival"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManifestFrom creates a Manifest from any Source
func ManifestFrom(c client.Client, src mf.Source, l Componentlogger) (m mf.Manifest, err error) {
	if l.DoLog(INFO_LEVEL) {
		return mf.ManifestFrom(src, mf.UseClient(mfc.NewClient(c)), mf.UseLogger(l.Logger.WithName("manifestival")))
	}
	return mf.ManifestFrom(src, mf.UseClient(mfc.NewClient(c)))
}
