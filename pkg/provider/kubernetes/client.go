/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	esv1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	"github.com/external-secrets/external-secrets/pkg/constants"
	"github.com/external-secrets/external-secrets/pkg/find"
	"github.com/external-secrets/external-secrets/pkg/metrics"
	"github.com/external-secrets/external-secrets/pkg/utils"
)

const (
	metaLabels      = "labels"
	metaAnnotations = "annotations"
)

func (c *Client) GetSecret(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, error) {
	secret, err := c.userSecretClient.Get(ctx, ref.Key, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// if property is not defined, we will return the json-serialized secret
	if ref.Property == "" {
		if ref.MetadataPolicy == esv1beta1.ExternalSecretMetadataPolicyFetch {
			m := map[string]map[string]string{}
			m[metaLabels] = secret.Labels
			m[metaAnnotations] = secret.Annotations

			j, err := utils.JSONMarshal(m)
			if err != nil {
				return nil, err
			}
			return j, nil
		}

		m := map[string]string{}
		for key, val := range secret.Data {
			m[key] = string(val)
		}
		j, err := utils.JSONMarshal(m)
		if err != nil {
			return nil, err
		}
		return j, nil
	}

	return getSecret(secret, ref)
}

func (c *Client) DeleteSecret(ctx context.Context, remoteRef esv1beta1.PushSecretRemoteRef) error {
	if remoteRef.GetProperty() == "" {
		return fmt.Errorf("requires property in RemoteRef to delete secret value")
	}

	extSecret, getErr := c.userSecretClient.Get(ctx, remoteRef.GetRemoteKey(), metav1.GetOptions{})
	metrics.ObserveAPICall(constants.ProviderKubernetes, constants.CallKubernetesGetSecret, getErr)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			// return gracefully if no secret exists
			return nil
		}
		return getErr
	}
	if _, ok := extSecret.Data[remoteRef.GetProperty()]; !ok {
		// return gracefully if specified secret does not contain the given property
		return nil
	}

	if len(extSecret.Data) > 1 {
		return c.removeProperty(ctx, extSecret, remoteRef)
	}
	return c.fullDelete(ctx, remoteRef.GetRemoteKey())
}

func (c *Client) SecretExists(_ context.Context, _ esv1beta1.PushSecretRemoteRef) (bool, error) {
	return false, fmt.Errorf("not implemented")
}

func (c *Client) PushSecret(ctx context.Context, localSecret *v1.Secret, remoteRef esv1beta1.PushSecretData) error {
	if remoteRef.GetProperty() == "" && remoteRef.GetSecretKey() != "" {
		return fmt.Errorf("requires property in RemoteRef to push secret value if secret key is defined")
	}

	remoteSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: c.store.RemoteNamespace,
			Name:      remoteRef.GetRemoteKey(),
		},
	}

	pushMeta, err := parseMetadataParameters(remoteRef.GetMetadata())
	if err != nil {
		return fmt.Errorf("unable to parse metadata parameters: %w", err)
	}

	err = c.createOrUpdate(ctx, remoteSecret, func() error {
		// merge metadata with existing metadata
		// The metadata in the remoteRef takes precedence, see below.
		if remoteSecret.ObjectMeta.Labels == nil {
			remoteSecret.ObjectMeta.Labels = make(map[string]string)
		}
		if remoteSecret.ObjectMeta.Annotations == nil {
			remoteSecret.ObjectMeta.Annotations = make(map[string]string)
		}
		utils.MergeStringMap(remoteSecret.ObjectMeta.Labels, localSecret.ObjectMeta.Labels)
		utils.MergeStringMap(remoteSecret.ObjectMeta.Annotations, localSecret.ObjectMeta.Annotations)

		// merge metadata from remoteRef
		if pushMeta != nil {
			if pushMeta.Spec.Labels != nil {
				utils.MergeStringMap(remoteSecret.ObjectMeta.Labels, pushMeta.Spec.Labels)
			}
			if pushMeta.Spec.Annotations != nil {
				utils.MergeStringMap(remoteSecret.ObjectMeta.Annotations, pushMeta.Spec.Annotations)
			}
		}

		// apply secret type
		secretType := v1.SecretTypeOpaque
		if localSecret.Type != "" {
			secretType = localSecret.Type
		}
		remoteSecret.Type = secretType

		// merge secret data with existing secret data
		if remoteSecret.Data == nil {
			remoteSecret.Data = make(map[string][]byte)
		}

		// if property is defined, we will only push to that property
		// if it is not defined (below), we will push the whole secret
		if remoteRef.GetProperty() != "" {
			// if secret key is empty, we will marshal the whole secret and put it into
			// the property defined in the remoteRef.
			if remoteRef.GetSecretKey() == "" {
				value, err := c.marshalData(localSecret)
				if err != nil {
					return err
				}
				remoteSecret.Data[remoteRef.GetProperty()] = value
			} else {
				// if secret key is defined, we will push that key from the local secret
				remoteSecret.Data[remoteRef.GetProperty()] = localSecret.Data[remoteRef.GetSecretKey()]
			}
		} else {
			for k, v := range localSecret.Data {
				remoteSecret.Data[k] = v
			}
		}
		return nil
	})
	metrics.ObserveAPICall(constants.ProviderKubernetes, constants.CallKubernetesGetSecret, err)
	return err
}

func (c *Client) createOrUpdate(ctx context.Context, targetSecret *v1.Secret, f func() error) error {
	target, err := c.userSecretClient.Get(ctx, targetSecret.Name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := f(); err != nil {
			return err
		}
		_, err := c.userSecretClient.Create(ctx, targetSecret, metav1.CreateOptions{})
		metrics.ObserveAPICall(constants.ProviderKubernetes, constants.CallKubernetesCreateSecret, err)
		if err != nil {
			return err
		}
		return nil
	}

	*targetSecret = *target
	existing := targetSecret.DeepCopyObject()
	if err := f(); err != nil {
		return err
	}

	if equality.Semantic.DeepEqual(existing, targetSecret) {
		return nil
	}

	_, err = c.userSecretClient.Update(ctx, targetSecret, metav1.UpdateOptions{})
	metrics.ObserveAPICall(constants.ProviderKubernetes, constants.CallKubernetesUpdateSecret, err)
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) marshalData(secret *v1.Secret) ([]byte, error) {
	values := make(map[string]string)
	for k, v := range secret.Data {
		values[k] = string(v)
	}

	// marshal
	value, err := utils.JSONMarshal(values)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal secrets into a single property: %w", err)
	}

	return value, nil
}

func (c *Client) GetSecretMap(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) (map[string][]byte, error) {
	secret, err := c.userSecretClient.Get(ctx, ref.Key, metav1.GetOptions{})
	metrics.ObserveAPICall(constants.ProviderKubernetes, constants.CallKubernetesGetSecret, err)
	if apierrors.IsNotFound(err) {
		return nil, esv1beta1.NoSecretError{}
	}
	if err != nil {
		return nil, err
	}
	var tmpMap map[string][]byte
	if ref.MetadataPolicy == esv1beta1.ExternalSecretMetadataPolicyFetch {
		tmpMap, err = getSecretMetadata(secret)
		if err != nil {
			return nil, err
		}
	} else {
		tmpMap = secret.Data
	}

	if ref.Property != "" {
		retMap, err := getPropertyMap(ref.Key, ref.Property, tmpMap)
		if err != nil {
			return nil, err
		}
		return retMap, nil
	}

	return tmpMap, nil
}

func getPropertyMap(key, property string, tmpMap map[string][]byte) (map[string][]byte, error) {
	byteArr, err := utils.JSONMarshal(tmpMap)
	if err != nil {
		return nil, err
	}
	var retMap map[string][]byte
	jsonStr := string(byteArr)
	// We need to search if a given key with a . exists before using gjson operations.
	idx := strings.Index(property, ".")
	if idx > -1 {
		refProperty := strings.ReplaceAll(property, ".", "\\.")
		retMap, err = getMapFromValues(refProperty, jsonStr)
		if err != nil {
			return nil, err
		}
		if retMap != nil {
			return retMap, nil
		}
	}
	retMap, err = getMapFromValues(property, jsonStr)
	if err != nil {
		return nil, err
	}
	if retMap == nil {
		return nil, fmt.Errorf("property %s does not exist in key %s", property, key)
	}
	return retMap, nil
}

func getMapFromValues(property, jsonStr string) (map[string][]byte, error) {
	val := gjson.Get(jsonStr, property)
	if val.Exists() {
		retMap := make(map[string][]byte)
		var tmpMap map[string]any
		decoded, err := base64.StdEncoding.DecodeString(val.String())
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(decoded, &tmpMap)
		if err != nil {
			return nil, err
		}
		for k, v := range tmpMap {
			b, err := utils.JSONMarshal(v)
			if err != nil {
				return nil, err
			}
			retMap[k] = b
		}
		return retMap, nil
	}
	return nil, nil
}

func getSecretMetadata(secret *v1.Secret) (map[string][]byte, error) {
	var err error
	tmpMap := make(map[string][]byte)
	tmpMap[metaLabels], err = utils.JSONMarshal(secret.ObjectMeta.Labels)
	if err != nil {
		return nil, err
	}
	tmpMap[metaAnnotations], err = utils.JSONMarshal(secret.ObjectMeta.Annotations)
	if err != nil {
		return nil, err
	}

	return tmpMap, nil
}

func (c *Client) GetAllSecrets(ctx context.Context, ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	if ref.Tags != nil {
		return c.findByTags(ctx, ref)
	}
	if ref.Name != nil {
		return c.findByName(ctx, ref)
	}
	return nil, fmt.Errorf("unexpected find operator: %#v", ref)
}

func (c *Client) findByTags(ctx context.Context, ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	// empty/nil tags = everything
	sel, err := labels.ValidatedSelectorFromSet(ref.Tags)
	if err != nil {
		return nil, fmt.Errorf("unable to validate selector tags: %w", err)
	}
	secrets, err := c.userSecretClient.List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	metrics.ObserveAPICall(constants.ProviderKubernetes, constants.CallKubernetesListSecrets, err)
	if err != nil {
		return nil, fmt.Errorf("unable to list secrets: %w", err)
	}
	data := make(map[string][]byte)
	for _, secret := range secrets.Items {
		jsonStr, err := utils.JSONMarshal(convertMap(secret.Data))
		if err != nil {
			return nil, err
		}
		data[secret.Name] = jsonStr
	}
	return utils.ConvertKeys(ref.ConversionStrategy, data)
}

func (c *Client) findByName(ctx context.Context, ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	secrets, err := c.userSecretClient.List(ctx, metav1.ListOptions{})
	metrics.ObserveAPICall(constants.ProviderKubernetes, constants.CallKubernetesListSecrets, err)
	if err != nil {
		return nil, fmt.Errorf("unable to list secrets: %w", err)
	}
	matcher, err := find.New(*ref.Name)
	if err != nil {
		return nil, err
	}
	data := make(map[string][]byte)
	for _, secret := range secrets.Items {
		if !matcher.MatchName(secret.Name) {
			continue
		}
		jsonStr, err := utils.JSONMarshal(convertMap(secret.Data))
		if err != nil {
			return nil, err
		}
		data[secret.Name] = jsonStr
	}
	return utils.ConvertKeys(ref.ConversionStrategy, data)
}

func (c *Client) Close(_ context.Context) error {
	return nil
}

func convertMap(in map[string][]byte) map[string]string {
	out := make(map[string]string)
	for k, v := range in {
		out[k] = string(v)
	}
	return out
}

// fullDelete removes remote secret completely.
func (c *Client) fullDelete(ctx context.Context, secretName string) error {
	err := c.userSecretClient.Delete(ctx, secretName, metav1.DeleteOptions{})
	metrics.ObserveAPICall(constants.ProviderKubernetes, constants.CallKubernetesDeleteSecret, err)

	// gracefully return on not found
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// removeProperty removes single data property from remote secret.
func (c *Client) removeProperty(ctx context.Context, extSecret *v1.Secret, remoteRef esv1beta1.PushSecretRemoteRef) error {
	delete(extSecret.Data, remoteRef.GetProperty())
	_, err := c.userSecretClient.Update(ctx, extSecret, metav1.UpdateOptions{})
	metrics.ObserveAPICall(constants.ProviderKubernetes, constants.CallKubernetesUpdateSecret, err)
	return err
}

func getSecret(secret *v1.Secret, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, error) {
	if ref.MetadataPolicy == esv1beta1.ExternalSecretMetadataPolicyFetch {
		s, found, err := getFromSecretMetadata(secret, ref)
		if err != nil {
			return nil, err
		}

		if !found {
			return nil, fmt.Errorf("property %s does not exist in metadata of secret %q", ref.Property, ref.Key)
		}

		return s, nil
	}

	s, found := getFromSecretData(secret, ref)
	if !found {
		return nil, fmt.Errorf("property %s does not exist in data of secret %q", ref.Property, ref.Key)
	}

	return s, nil
}

func getFromSecretData(secret *v1.Secret, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, bool) {
	// Check if a property with "." exists first such as file.png
	v, ok := secret.Data[ref.Property]
	if ok {
		return v, true
	}

	idx := strings.Index(ref.Property, ".")
	if idx == -1 || idx == 0 || idx == len(ref.Property)-1 {
		return nil, false
	}

	v, ok = secret.Data[ref.Property[:idx]]
	if !ok {
		return nil, false
	}

	val := gjson.Get(string(v), ref.Property[idx+1:])
	if !val.Exists() {
		return nil, false
	}

	return []byte(val.String()), true
}

func getFromSecretMetadata(secret *v1.Secret, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, bool, error) {
	path := strings.Split(ref.Property, ".")

	var metadata map[string]string
	switch path[0] {
	case metaLabels:
		metadata = secret.Labels
	case metaAnnotations:
		metadata = secret.Annotations
	default:
		return nil, false, nil
	}

	if len(path) == 1 {
		j, err := utils.JSONMarshal(metadata)
		if err != nil {
			return nil, false, err
		}
		return j, true, nil
	}

	v, ok := metadata[path[1]]
	if !ok {
		return nil, false, nil
	}
	if len(path) == 2 {
		return []byte(v), true, nil
	}

	val := gjson.Get(v, strings.Join(path[2:], ""))
	if !val.Exists() {
		return nil, false, nil
	}

	return []byte(val.String()), true, nil
}

const (
	metadataAPIVersion = "kubernetes.external-secrets.io/v1alpha1"
	metadataKind       = "PushSecretMetadata"
)

type PushSecretMetadata struct {
	metav1.TypeMeta
	Spec PushSecretMetadataSpec `json:"spec,omitempty"`
}
type PushSecretMetadataSpec struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func parseMetadataParameters(data *apiextensionsv1.JSON) (*PushSecretMetadata, error) {
	if data == nil {
		return nil, nil
	}
	var metadata PushSecretMetadata
	err := yaml.Unmarshal(data.Raw, &metadata, yaml.DisallowUnknownFields)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s %s: %w", metadataAPIVersion, metadataKind, err)
	}

	if metadata.APIVersion != metadataAPIVersion {
		return nil, fmt.Errorf("unexpected apiVersion %q, expected %q", metadata.APIVersion, metadataAPIVersion)
	}

	if metadata.Kind != metadataKind {
		return nil, fmt.Errorf("unexpected kind %q, expected %q", metadata.Kind, metadataKind)
	}

	return &metadata, nil
}
