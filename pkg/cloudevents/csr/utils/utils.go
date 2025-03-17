package utils

import (
	"encoding/json"
	"fmt"

	jsonpatch "github.com/evanphx/json-patch"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	certificatev1 "k8s.io/api/certificates/v1"
)

// Patch applies the patch to a csr with the patch type.
func Patch(patchType types.PatchType, csr *certificatev1.CertificateSigningRequest, patchData []byte) (*certificatev1.CertificateSigningRequest, error) {
	csrData, err := json.Marshal(csr)
	if err != nil {
		return nil, err
	}

	var patchedData []byte
	switch patchType {
	case types.JSONPatchType:
		var patchObj jsonpatch.Patch
		patchObj, err = jsonpatch.DecodePatch(patchData)
		if err != nil {
			return nil, err
		}
		patchedData, err = patchObj.Apply(csrData)
		if err != nil {
			return nil, err
		}

	case types.MergePatchType:
		patchedData, err = jsonpatch.MergePatch(csrData, patchData)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported patch type: %s", patchType)
	}

	patchedCSR := &certificatev1.CertificateSigningRequest{}
	if err := json.Unmarshal(patchedData, patchedCSR); err != nil {
		return nil, err
	}

	return patchedCSR, nil
}

// ListCSRsWithOptions retrieves the csrs from store which matches the options.
func ListCSRsWithOptions(store cache.Store, opts metav1.ListOptions) ([]*certificatev1.CertificateSigningRequest, error) {
	var err error

	labelSelector := labels.Everything()
	fieldSelector := fields.Everything()

	if len(opts.LabelSelector) != 0 {
		labelSelector, err = labels.Parse(opts.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid labels selector %q: %v", opts.LabelSelector, err)
		}
	}

	if len(opts.FieldSelector) != 0 {
		fieldSelector, err = fields.ParseSelector(opts.FieldSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid fields selector %q: %v", opts.FieldSelector, err)
		}
	}

	csrs := []*certificatev1.CertificateSigningRequest{}
	// list with labels
	if err := cache.ListAll(store, labelSelector, func(obj interface{}) {
		csr, ok := obj.(*certificatev1.CertificateSigningRequest)
		if !ok {
			return
		}

		csrFieldSet := fields.Set{
			"metadata.name": csr.Name,
		}

		if !fieldSelector.Matches(csrFieldSet) {
			return
		}

		csrs = append(csrs, csr)
	}); err != nil {
		return nil, err
	}

	return csrs, nil
}
