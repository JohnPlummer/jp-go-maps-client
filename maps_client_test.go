package mapsclient_test

import (
	"context"
	"errors"
	"testing"
	"time"

	mapsclient "github.com/JohnPlummer/jp-go-maps-client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMapsClient(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Maps Client Suite")
}

var _ = Describe("APIError", func() {
	Describe("ParseAPIError", func() {
		It("returns nil for nil error", func() {
			result := mapsclient.ParseAPIError(nil)
			Expect(result).To(BeNil())
		})

		It("returns nil for non-Maps error", func() {
			err := errors.New("some random error")
			result := mapsclient.ParseAPIError(err)
			Expect(result).To(BeNil())
		})

		It("parses Maps API error with status code and message", func() {
			err := errors.New("maps: OVER_QUERY_LIMIT - You have exceeded your daily request quota for this API.")
			result := mapsclient.ParseAPIError(err)

			Expect(result).NotTo(BeNil())
			Expect(result.StatusCode).To(Equal("OVER_QUERY_LIMIT"))
			Expect(result.Message).To(Equal("You have exceeded your daily request quota for this API."))
			Expect(result.Err).To(Equal(err))
		})

		It("parses Maps API error without separator", func() {
			err := errors.New("maps: unknown error format")
			result := mapsclient.ParseAPIError(err)

			Expect(result).NotTo(BeNil())
			Expect(result.StatusCode).To(BeEmpty())
			Expect(result.Message).To(Equal("unknown error format"))
		})

		It("parses REQUEST_DENIED error", func() {
			err := errors.New("maps: REQUEST_DENIED - API key is invalid.")
			result := mapsclient.ParseAPIError(err)

			Expect(result).NotTo(BeNil())
			Expect(result.StatusCode).To(Equal("REQUEST_DENIED"))
			Expect(result.Message).To(Equal("API key is invalid."))
		})

		It("parses ZERO_RESULTS error", func() {
			err := errors.New("maps: ZERO_RESULTS - No results found.")
			result := mapsclient.ParseAPIError(err)

			Expect(result).NotTo(BeNil())
			Expect(result.StatusCode).To(Equal("ZERO_RESULTS"))
		})
	})

	Describe("Error()", func() {
		It("formats error with status code", func() {
			apiErr := &mapsclient.APIError{
				StatusCode: "OVER_QUERY_LIMIT",
				Message:    "Quota exceeded",
			}
			Expect(apiErr.Error()).To(Equal("Maps API error [status: OVER_QUERY_LIMIT]: Quota exceeded"))
		})

		It("formats error without status code", func() {
			apiErr := &mapsclient.APIError{
				Message: "Unknown error",
			}
			Expect(apiErr.Error()).To(Equal("Maps API error: Unknown error"))
		})
	})

	Describe("Unwrap()", func() {
		It("returns the underlying error", func() {
			originalErr := errors.New("original")
			apiErr := &mapsclient.APIError{
				Err: originalErr,
			}
			Expect(apiErr.Unwrap()).To(Equal(originalErr))
		})
	})
})

var _ = Describe("New", func() {
	It("returns error for empty API key", func() {
		client, err := mapsclient.New("")

		Expect(client).To(BeNil())
		Expect(err).To(Equal(mapsclient.ErrInvalidAPIKey))
	})

	It("creates client with valid API key", func() {
		client, err := mapsclient.New("test-api-key")

		Expect(err).NotTo(HaveOccurred())
		Expect(client).NotTo(BeNil())

		err = client.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("applies custom options", func() {
		client, err := mapsclient.New("test-api-key",
			mapsclient.WithTimeout(5*time.Second),
			mapsclient.WithCacheTTL(1*time.Hour),
			mapsclient.WithCleanupInterval(30*time.Minute),
		)

		Expect(err).NotTo(HaveOccurred())
		Expect(client).NotTo(BeNil())

		err = client.Close()
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("Client", func() {
	var client mapsclient.Client

	BeforeEach(func() {
		var err error
		client, err = mapsclient.New("test-api-key",
			mapsclient.WithTimeout(100*time.Millisecond),
		)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if client != nil {
			client.Close()
		}
	})

	Describe("Geocode", func() {
		It("returns error for empty address", func() {
			result, err := client.Geocode(context.Background(), "")

			Expect(result).To(BeNil())
			Expect(err).To(Equal(mapsclient.ErrEmptyLocation))
		})

		It("returns error for whitespace-only address", func() {
			result, err := client.Geocode(context.Background(), "   ")

			Expect(result).To(BeNil())
			Expect(err).To(Equal(mapsclient.ErrEmptyLocation))
		})
	})

	Describe("GeocodeWithOptions", func() {
		It("returns error for empty address with options", func() {
			opts := &mapsclient.GeocodeOptions{
				Region:   "uk",
				Language: "en",
			}
			result, err := client.GeocodeWithOptions(context.Background(), "", opts)

			Expect(result).To(BeNil())
			Expect(err).To(Equal(mapsclient.ErrEmptyLocation))
		})
	})

	Describe("State", func() {
		It("returns circuit breaker state", func() {
			state := client.State()
			// Initial state should be closed
			Expect(state.String()).To(Equal("closed"))
		})
	})

	Describe("Counts", func() {
		It("returns circuit breaker counts", func() {
			counts := client.Counts()
			// Initial counts should be zero
			Expect(counts.Requests).To(Equal(uint32(0)))
			Expect(counts.TotalSuccesses).To(Equal(uint32(0)))
			Expect(counts.TotalFailures).To(Equal(uint32(0)))
		})
	})
})

var _ = Describe("GeocodeResult", func() {
	It("has expected fields", func() {
		result := &mapsclient.GeocodeResult{
			Address:    "1600 Amphitheatre Parkway, Mountain View, CA",
			Latitude:   37.4224764,
			Longitude:  -122.0842499,
			Confidence: 1.0,
			PlaceID:    "ChIJ2eUgeAK6j4ARbn5u_wAGqWA",
			Types:      []string{"street_address"},
		}

		Expect(result.Address).To(Equal("1600 Amphitheatre Parkway, Mountain View, CA"))
		Expect(result.Latitude).To(BeNumerically("~", 37.42, 0.01))
		Expect(result.Longitude).To(BeNumerically("~", -122.08, 0.01))
		Expect(result.Confidence).To(Equal(1.0))
		Expect(result.PlaceID).NotTo(BeEmpty())
		Expect(result.Types).To(ContainElement("street_address"))
	})
})

var _ = Describe("GeocodeOptions", func() {
	It("supports region option", func() {
		opts := &mapsclient.GeocodeOptions{
			Region: "uk",
		}
		Expect(opts.Region).To(Equal("uk"))
	})

	It("supports language option", func() {
		opts := &mapsclient.GeocodeOptions{
			Language: "en",
		}
		Expect(opts.Language).To(Equal("en"))
	})
})
