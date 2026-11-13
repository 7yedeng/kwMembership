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
	if contract.ID == "" || contract.Version < 1 || contract.Currency != "PHP" ||
		math.IsNaN(contract.MinAmount) || math.IsInf(contract.MinAmount, 0) ||
		math.IsNaN(contract.MaxAmount) || math.IsInf(contract.MaxAmount, 0) ||
		contract.MinAmount <= 0 || contract.MaxAmount < contract.MinAmount {
		return failure("CHECKOUT_REQUEST_INVALID", "checkout price contract is invalid")
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
