package utils

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"

	kabanerov1alpha2 "github.com/kabanero-io/kabanero-operator/pkg/apis/kabanero/v1alpha2"
	ologger "github.com/kabanero-io/kabanero-operator/pkg/controller/logger"
	"github.com/kabanero-io/kabanero-operator/pkg/controller/utils/cache"
	yml "gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ualog = ologger.NewOperatorlogger("controller.utils.archive")

// Stack archive manifest.yaml
type StackManifest struct {
	Contents []StackContents `yaml:"contents,omitempty"`
}

type StackContents struct {
	File   string `yaml:"file,omitempty"`
	Sha256 string `yaml:"sha256,omitempty"`
}

// This is the rendered asset, including its sha256 from the manifest.
type StackAsset struct {
	Name    string
	Group   string
	Version string
	Kind    string
	Sha256  string
	Yaml    unstructured.Unstructured
}

func DownloadToByte(c client.Client, namespace string, url string, gitRelease kabanerov1alpha2.GitReleaseInfo, skipCertVerification bool) ([]byte, error) {
	var archiveBytes []byte
	switch {
	// GIT:
	case gitRelease.IsUsable():
		bytes, err := cache.GetStackDataUsingGit(c, gitRelease, skipCertVerification, namespace)
		if err != nil {
			return nil, err
		}
		archiveBytes = bytes
	// HTTPS:
	case len(url) != 0:
		bytes, err := cache.GetFromCache(c, url, skipCertVerification)
		if err != nil {
			return nil, err
		}
		archiveBytes = bytes
	// NOT SUPPORTED:
	default:
		return nil, fmt.Errorf("No information was provided to retrieve the stack's index file. Specify a stack repository that includes a HTTP URL location or GitHub release information.")
	}

	return archiveBytes, nil
}

// Print something that looks similar to xxd output
func commTrace(buffer []byte) string {
	var sb strings.Builder
	for bytesLeft := len(buffer); bytesLeft > 0; {
		var bytesThisRound []byte
		if bytesLeft >= 16 {
			bytesThisRound = buffer[len(buffer)-bytesLeft : len(buffer)-bytesLeft+16]
		} else {
			bytesThisRound = buffer[len(buffer)-bytesLeft:]
		}

		// Build up the line to print
		sb.WriteString(fmt.Sprintf("%.08X: ", len(buffer)-bytesLeft))
		for i := 0; i < 16; i = i + 2 {
			x := len(bytesThisRound) - i
			if x >= 2 {
				sb.WriteString(fmt.Sprintf("%.04X ", bytesThisRound[i:i+2]))
			} else if x == 1 {
				sb.WriteString(fmt.Sprintf("%.02X   ", bytesThisRound[i]))
			} else {
				sb.WriteString("     ")
			}
		}

		for _, b := range bytesThisRound {
			if unicode.IsPrint(rune(b)) {
				sb.WriteByte(b)
			} else {
				sb.WriteString(".")
			}
		}
		sb.WriteString("\n")

		// Subtract for next loop
		bytesLeft -= len(bytesThisRound)
	}

	return sb.String()
}

// Read X bytes from reader.
func readBytesFromReader(size int64, r io.Reader) ([]byte, error) {
	b := make([]byte, size)
	for bytesLeft := size; bytesLeft > 0; {
		i, err := r.Read(b[size-bytesLeft:])
		bytesLeft -= int64(i)
		// An EOF error is normal as long as we read all the bytes.
		if err != nil {
			if err == io.EOF {
				if bytesLeft != 0 {
					return nil, fmt.Errorf("EOF received before end of file: %v", err.Error())
				}

				break
			}

			// Otherwise, just return the error.
			return nil, err
		}
	}

	return b, nil
}

//Read the manifests from a tar.gz archive
//It would be better to use the manifest.yaml as the index, and check the signatures
//For now, ignore manifest.yaml and return all other yaml files from the archive
func decodeManifests(archive []byte, renderingContext map[string]interface{}) ([]StackAsset, error) {
	manifests := []StackAsset{}
	var stackmanifest StackManifest

	// Read the manifest.yaml from the stack archive
	r := bytes.NewReader(archive)
	gzReader, err := gzip.NewReader(r)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Could not read manifest gzip"))
	}
	tarReader := tar.NewReader(gzReader)

	foundManifest := false
	var headers []string
	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, errors.New(fmt.Sprintf("Could not read manifest tar"))
		}

		headers = append(headers, header.Name)

		switch {
		case strings.TrimPrefix(header.Name, "./") == "manifest.yaml":
			//Buffer the document for further processing
			b, err := readBytesFromReader(header.Size, tarReader)
			if err != nil {
				return nil, fmt.Errorf("Error reading archive %v: %v", header.Name, err.Error())
			}
			err = yml.Unmarshal(b, &stackmanifest)
			if err != nil {
				return nil, err
			}
			foundManifest = true
		}
	}

	ualog.Info(fmt.Sprintf("Header names: %v", strings.Join(headers, ",")))

	if foundManifest != true {
		return nil, fmt.Errorf("Error reading archive, unable to read manifest.yaml")
	}

	// Re-Read the archive and validate against archive manifest.yaml
	r = bytes.NewReader(archive)
	gzReader, err = gzip.NewReader(r)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Could not read manifest gzip"))
	}
	tarReader = tar.NewReader(gzReader)

	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, errors.New(fmt.Sprintf("Could not read manifest tar"))
		}

		// Ignore manifest.yaml on this pass, only read yaml files
		switch {
		case strings.TrimPrefix(header.Name, "./") == "manifest.yaml":
			break
		case strings.HasSuffix(header.Name, ".yaml"):
			//Buffer the document for further processing
			b, err := readBytesFromReader(header.Size, tarReader)
			if err != nil {
				return nil, fmt.Errorf("Error reading archive %v: %v", header.Name, err.Error())
			}

			// Checksum. Lookup the read file in the index and compare sha256
			match := false
			b_sum := sha256.Sum256(b)
			assetSumString := ""
			for _, content := range stackmanifest.Contents {
				if content.File == strings.TrimPrefix(header.Name, "./") {
					// Older releases may not have a sha256 in the manifest.yaml
					assetSumString = content.Sha256
					if content.Sha256 != "" {
						var c_sum [32]byte
						decoded, err := hex.DecodeString(content.Sha256)
						if err != nil {
							return nil, err
						}
						copy(c_sum[:], decoded)
						if b_sum != c_sum {
							return nil, fmt.Errorf("Archive file: %v  manifest.yaml checksum: %x  did not match file checksum: %x", header.Name, c_sum, b_sum)
						}
						match = true
					} else {
						// Would be nice if we could make this a warning message, but it seems like the only
						// options are error and info.  It's possible that some implementation has other methods
						// but someone needs to investigate.
						ualog.Info(fmt.Sprintf("Archive file %v was listed in the manifest but had no checksum.  Checksum validation for this file is skipped.", header.Name))
						match = true
					}
				}
			}
			if match != true {
				return nil, fmt.Errorf("File %v was found in the archive, but not in the manifest.yaml", header.Name)
			}

			//Apply the Kabanero yaml directive processor
			pmanifests, err := processManifest(b, renderingContext, header.Name, assetSumString)
			if (err != nil) && (err != io.EOF) {
				return nil, fmt.Errorf("Error decoding %v: %v", header.Name, err.Error())
			}
			manifests = append(manifests, pmanifests...)
		}
	}
	return manifests, nil
}

//Apply the Kabanero yaml directive processor
func processManifest(b []byte, renderingContext map[string]interface{}, filename string, assetSumString string) ([]StackAsset, error) {
	manifests := []StackAsset{}
	s := &DirectiveProcessor{}
	rb, err := s.Render(b, renderingContext)
	if err != nil {
		return manifests, fmt.Errorf("Error processing directives %v: %v", filename, err.Error())
	}

	decoder := yaml.NewYAMLToJSONDecoder(bytes.NewReader(rb))
	out := unstructured.Unstructured{}
	for err = decoder.Decode(&out); err == nil; {
		gvk := out.GroupVersionKind()
		manifests = append(manifests, StackAsset{Name: out.GetName(), Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Yaml: out, Sha256: assetSumString})
		out = unstructured.Unstructured{}
		err = decoder.Decode(&out)
	}
	return manifests, err
}

type fileType string

var tarGzType fileType = ".tar.gz"
var yamlType fileType = ".yaml"

func getPipelineFileType(pipelineStatus kabanerov1alpha2.PipelineStatus) (fileType, error) {
	fileNameURL, err := url.Parse(pipelineStatus.Url)
	if err != nil {
		return "", err
	}
	fileName := fileNameURL.Path
	if pipelineStatus.GitRelease.IsUsable() {
		fileName = pipelineStatus.GitRelease.AssetName
	}
	switch {
	case strings.HasSuffix(fileName, ".tar.gz") || strings.HasSuffix(fileName, ".tgz"):
		return tarGzType, nil
	case strings.HasSuffix(fileName, ".yaml") || strings.HasSuffix(fileName, ".yml"):
		return yamlType, nil
	default:
		return "", nil
	}
}

func GetManifests(c client.Client, namespace string, pipelineStatus kabanerov1alpha2.PipelineStatus, renderingContext map[string]interface{}, skipCertVerification bool) ([]StackAsset, error) {
	b, err := DownloadToByte(c, namespace, pipelineStatus.Url, pipelineStatus.GitRelease, skipCertVerification)
	if err != nil {
		return nil, err
	}

	b_sum := sha256.Sum256(b)
	var c_sum [32]byte
	decoded, err := hex.DecodeString(pipelineStatus.Digest)
	if err != nil {
		return nil, err
	}
	copy(c_sum[:], decoded)

	fileType, err := getPipelineFileType(pipelineStatus)
	if err != nil {
		return nil, err
	}
	if fileType == tarGzType {
		if b_sum != c_sum {
			return nil, fmt.Errorf("Index checksum: %x not match download checksum: %x for Pipeline Name %v", c_sum, b_sum, pipelineStatus.Name)
		}
		manifests, err := decodeManifests(b, renderingContext)
		if err != nil {
			return nil, err
		}
		return manifests, nil
	} else if fileType == yamlType {
		if b_sum != c_sum {
			ualog.Info(fmt.Sprintf("Index checksum: %x not match download checksum: %x for Pipeline Name %v", c_sum, b_sum, pipelineStatus.Name))
		}
		manifests, err := processManifest(b, renderingContext, pipelineStatus.Name, hex.EncodeToString(b_sum[:]))
		if (err != nil) && (err != io.EOF) {
			return nil, err
		}
		return manifests, nil
	}

	return nil, fmt.Errorf("Can not decode file type of file for Pipeline %v. Must be .tar.gz or .yaml.", pipelineStatus.Name)
}
