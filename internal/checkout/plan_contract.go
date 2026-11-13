package checkout

import "math"

// ValidatePlanContract checks the supported stage, target and price contract.
// Both the Chrome executor and the Python bridge use this before creating any
// browser or durable command. It does not grant a payment permit or validate
// credentials, material, a workflow binding or the actual checkout page.
func ValidatePlanContract(request Request) error {
	interactiveGo := request.Mode == ModeInteractivePreflight && request.Stage == "go" && request.TargetTier == "go"
	if request.Stage != "plus" && request.Stage != "upgrade" && !interactiveGo {
		return failure("CHECKOUT_REQUEST_INVALID", "checkout stage is invalid")
	}
	if !map[string]bool{"plus": true, "x5": true, "x20": true}[request.TargetTier] && !interactiveGo {
		return failure("CHECKOUT_REQUEST_INVALID", "target membership tier is invalid")
	}
	contract := request.PriceContract
	if contract.ID == "" || contract.Version < 1 ||
		math.IsNaN(contract.MinAmount) || math.IsInf(contract.MinAmount, 0) ||
		math.IsNaN(contract.MaxAmount) || math.IsInf(contract.MaxAmount, 0) ||
		contract.MinAmount <= 0 || contract.MaxAmount < contract.MinAmount {
		return failure("CHECKOUT_REQUEST_INVALID", "checkout price contract is invalid")
	}
	if _, _, err := ExpectedCheckoutMarket(request); err != nil {
		return err
	}
	// Higher-tier orders first purchase Plus. Price follows the current stage,
	// not the eventual target membership.
	expectedTier := "plus"
	if interactiveGo {
		expectedTier = "go"
	} else if request.Stage == "upgrade" {
		if request.TargetTier != "x5" && request.TargetTier != "x20" {
			return failure("CHECKOUT_REQUEST_INVALID", "upgrade target membership tier is invalid")
		}
		expectedTier = request.TargetTier
	}
	if contract.Tier != expectedTier {
		return failure("CHECKOUT_REQUEST_INVALID", "price contract does not match the checkout stage")
	}
	return nil
}

// ExpectedCheckoutMarket returns the explicit checkout country/currency pair.
// Only the original PHP contract may omit country for compatibility. Egypt is
// available solely to the tokenless, non-payment Go observation path; it does
// not change the automatic Plus API payload or enable a production Go order.
func ExpectedCheckoutMarket(request Request) (country, currency string, err error) {
	country, currency = request.PriceContract.Country, request.PriceContract.Currency
	if country == "" && currency == "PHP" {
		country = "PH"
	}
	if country == "PH" && currency == "PHP" {
		return country, currency, nil
	}
	if country == "EG" && currency == "EGP" && request.Mode == ModeInteractivePreflight &&
		request.Stage == "go" && request.TargetTier == "go" && request.PriceContract.Tier == "go" {
		return country, currency, nil
	}
	return "", "", failure("CHECKOUT_REQUEST_INVALID", "checkout country and currency are unsupported for this mode")
}
