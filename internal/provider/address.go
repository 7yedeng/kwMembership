package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

type AddressClient struct {
	http    *http.Client
	baseURL string
}

type Cardholder struct {
	FirstName string
	LastName  string
}

type BillingAddress struct {
	Name       string
	Phone      string
	Line1      string
	City       string
	State      string
	PostalCode string
	Country    string
}

type addressItem struct {
	FirstName  string
	LastName   string
	Name       string
	Phone      string
	Line1      string
	City       string
	State      string
	PostalCode string
	Country    string
}

func NewAddressClient(client *http.Client, baseURL string) *AddressClient {
	return &AddressClient{http: client, baseURL: strings.TrimRight(baseURL, "/")}
}

func (c *AddressClient) Cardholder(ctx context.Context) (Cardholder, error) {
	item, err := c.load(ctx)
	if err != nil {
		return Cardholder{}, err
	}
	holder := Cardholder{FirstName: item.FirstName, LastName: item.LastName}
	if holder.FirstName == "" || holder.LastName == "" || len(holder.FirstName) > 100 || len(holder.LastName) > 100 {
		return Cardholder{}, fail("NEW_CARD_HOLDER_UNAVAILABLE", "cardholder is incomplete", true)
	}
	return holder, nil
}

func (c *AddressClient) Billing(ctx context.Context) (BillingAddress, error) {
	item, err := c.load(ctx)
	if err != nil {
		return BillingAddress{}, err
	}
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = strings.TrimSpace(item.FirstName + " " + item.LastName)
	}
	address := BillingAddress{
		Name: name, Phone: item.Phone, Line1: item.Line1, City: item.City,
		State: strings.ToUpper(item.State), PostalCode: item.PostalCode,
		Country: strings.ToUpper(item.Country),
	}
	if len(address.Name) < 2 || len(address.Name) > 120 || len(address.Line1) < 2 || len(address.Line1) > 160 ||
		len(address.City) < 2 || len(address.City) > 100 || address.State != "DE" || address.Country != "US" ||
		len(address.PostalCode) < 5 || len(address.PostalCode) > 10 {
		return BillingAddress{}, fail("CHECKOUT_ADDRESS_UNAVAILABLE", "billing address response is incomplete", true)
	}
	return address, nil
}

func (c *AddressClient) load(ctx context.Context) (addressItem, error) {
	params := url.Values{"count": {"1"}, "includePerson": {"true"}, "state": {"DE"}}
	request, err := newRequest(ctx, http.MethodGet, c.baseURL+"/api/public/us-address/generate?"+params.Encode(), nil)
	if err != nil {
		return addressItem{}, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return addressItem{}, wrap("NEW_CARD_HOLDER_UNAVAILABLE", "address provider unavailable", err, true)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return addressItem{}, fail("NEW_CARD_HOLDER_UNAVAILABLE", "address provider rejected request", true)
	}
	raw, err := readLimited(response, maxMembershipResponse, "ADDRESS_RESPONSE_TOO_LARGE")
	if err != nil {
		return addressItem{}, err
	}
	var payload struct {
		Items []struct {
			Person struct {
				FirstName string `json:"firstName"`
				LastName  string `json:"lastName"`
				Name      string `json:"name"`
				Phone     string `json:"phone"`
			} `json:"person"`
			Address struct {
				Street      string `json:"street"`
				Line1       string `json:"line1"`
				City        string `json:"city"`
				State       string `json:"state"`
				StateAbbr   string `json:"stateAbbr"`
				ZipCode     string `json:"zipCode"`
				PostalCode  string `json:"postalCode"`
				CountryCode string `json:"countryCode"`
			} `json:"address"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &payload) != nil || len(payload.Items) != 1 {
		return addressItem{}, fail("NEW_CARD_HOLDER_UNAVAILABLE", "address response contract drift", true)
	}
	item := payload.Items[0]
	line1 := strings.TrimSpace(item.Address.Street)
	if line1 == "" {
		line1 = strings.TrimSpace(item.Address.Line1)
	}
	state := strings.TrimSpace(item.Address.StateAbbr)
	if state == "" {
		state = strings.TrimSpace(item.Address.State)
	}
	postal := strings.TrimSpace(item.Address.ZipCode)
	if postal == "" {
		postal = strings.TrimSpace(item.Address.PostalCode)
	}
	return addressItem{
		FirstName: strings.TrimSpace(item.Person.FirstName), LastName: strings.TrimSpace(item.Person.LastName),
		Name: strings.TrimSpace(item.Person.Name), Phone: strings.TrimSpace(item.Person.Phone),
		Line1: line1, City: strings.TrimSpace(item.Address.City), State: state,
		PostalCode: postal, Country: strings.TrimSpace(item.Address.CountryCode),
	}, nil
}
