package voteverifier

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
	"github.com/consensys/gnark/profile"
	"github.com/consensys/gnark/std/math/emulated"
	gecdsa "github.com/consensys/gnark/std/signature/ecdsa"
	"github.com/consensys/gnark/test"
	qt "github.com/frankban/quicktest"
	"github.com/iden3/go-iden3-crypto/mimc7"
	arbotree "github.com/vocdoni/arbo"
	"github.com/vocdoni/circom2gnark/parser"
	internaltest "github.com/vocdoni/gnark-crypto-primitives/test"
	"github.com/vocdoni/vocdoni-z-sandbox/encrypt"
	"go.vocdoni.io/dvote/util"
)

const (
	n_fields = 8
	n_levels = 160
)

var (
	ballotProofWasm = "../assets/circom/circuit/ballot_proof.wasm"
	ballotProofPKey = "../assets/circom/circuit/ballot_proof_pkey.zkey"
	ballotProofVKey = "../assets/circom/circuit/ballot_proof_vkey.json"

	maxCount        = 5
	forceUniqueness = 0
	maxValue        = 16
	minValue        = 0
	costExp         = 2
	costFromWeight  = 0
	weight          = 10
	fields          = GenerateBallotFields(maxCount, maxValue, minValue, forceUniqueness > 0)
)

func TestVerifyVoteCircuit(t *testing.T) {
	c := qt.New(t)

	// CLIENT SIDE CIRCOM CIRCUIT

	// generate encryption key and user nonce k
	encryptionKey := GenerateEncryptionTestKey()
	encryptionKeyX, encryptionKeyY := encryptionKey.Point()
	k, err := encrypt.RandK()
	c.Assert(err, qt.IsNil)
	// encrypt the ballots
	cipherfields, plainCipherfields := CipherBallotFields(fields, n_fields, encryptionKey, k)
	// generate voter account
	privKey, pubKey, address, err := GenerateECDSAaccount()
	c.Assert(err, qt.IsNil)
	// generate the commitment
	processID := util.RandomBytes(20)
	secret := util.RandomBytes(16)
	commitment, nullifier, err := MockedCommitmentAndNullifier(address.Bytes(), processID, secret)
	c.Assert(err, qt.IsNil)
	// group the circom inputs to hash
	bigCircomInputs := []*big.Int{
		big.NewInt(int64(maxCount)),
		big.NewInt(int64(forceUniqueness)),
		big.NewInt(int64(maxValue)),
		big.NewInt(int64(minValue)),
		big.NewInt(int64(math.Pow(float64(maxValue), float64(costExp))) * int64(maxCount)),
		big.NewInt(int64(maxCount)),
		big.NewInt(int64(costExp)),
		big.NewInt(int64(costFromWeight)),
		util.BigToFF(new(big.Int).SetBytes(address.Bytes())),
		big.NewInt(int64(weight)),
		util.BigToFF(new(big.Int).SetBytes(processID)),
		encryptionKeyX,
		encryptionKeyY,
		nullifier,
		commitment,
	}
	bigCircomInputs = append(bigCircomInputs, plainCipherfields...)
	circomInputsHash, err := mimc7.Hash(bigCircomInputs, nil)
	c.Assert(err, qt.IsNil)
	// sign the inputs hash
	rSign, sSign, err := SignECDSA(privKey, circomInputsHash.Bytes())
	c.Assert(err, qt.IsNil)
	// init circom inputs
	circomInputs := map[string]any{
		"fields":           BigIntArrayToStringArray(fields, n_fields),
		"max_count":        fmt.Sprint(maxCount),
		"force_uniqueness": fmt.Sprint(forceUniqueness),
		"max_value":        fmt.Sprint(maxValue),
		"min_value":        fmt.Sprint(minValue),
		"max_total_cost":   fmt.Sprint(int(math.Pow(float64(maxValue), float64(costExp))) * maxCount), // (maxValue-1)^costExp * maxCount
		"min_total_cost":   fmt.Sprint(maxCount),
		"cost_exp":         fmt.Sprint(costExp),
		"cost_from_weight": fmt.Sprint(costFromWeight),
		"address":          util.BigToFF(new(big.Int).SetBytes(address.Bytes())).String(),
		"weight":           fmt.Sprint(weight),
		"process_id":       util.BigToFF(new(big.Int).SetBytes(processID)).String(),
		"pk":               []string{encryptionKeyX.String(), encryptionKeyY.String()},
		"k":                k.String(),
		"cipherfields":     cipherfields,
		"nullifier":        nullifier.String(),
		"commitment":       commitment.String(),
		"secret":           util.BigToFF(new(big.Int).SetBytes(secret)).String(),
		"inputs_hash":      circomInputsHash.String(),
	}
	bCircomInputs, err := json.Marshal(circomInputs)
	c.Assert(err, qt.IsNil)
	// create the proof
	circomProof, pubSignals, err := CompileAndGenerateProof(bCircomInputs, ballotProofWasm, ballotProofPKey)
	c.Assert(err, qt.IsNil)
	// transform cipherfields to gnark frontend.Variable
	fBallots := [n_fields][2][2]frontend.Variable{}
	for i, c := range cipherfields {
		fBallots[i] = [2][2]frontend.Variable{
			{
				frontend.Variable(c[0][0]),
				frontend.Variable(c[0][1]),
			},
			{
				frontend.Variable(c[1][0]),
				frontend.Variable(c[1][1]),
			},
		}
	}
	// generate a test census proof
	testCensus, err := internaltest.GenerateCensusProofForTest(internaltest.CensusTestConfig{
		Dir:           "../assets/census",
		ValidSiblings: 10,
		TotalSiblings: n_levels,
		KeyLen:        20,
		Hash:          arbotree.HashFunctionMiMC_BLS12_377,
		BaseFiled:     arbotree.BLS12377BaseField,
	}, address.Bytes(), new(big.Int).SetInt64(int64(weight)).Bytes())
	c.Assert(err, qt.IsNil)
	// transform siblings to gnark frontend.Variable
	fSiblings := [n_levels]frontend.Variable{}
	for i, s := range testCensus.Siblings {
		fSiblings[i] = frontend.Variable(s)
	}
	// init the vote gnark inputs to hash them (circom + census root)
	bigGnarkInputs := append(bigCircomInputs, testCensus.Root)
	// hash the inputs
	inputsHash, err := mimc7.Hash(bigGnarkInputs, nil)
	c.Assert(err, qt.IsNil)
	// parse input files
	proof, placeHolders, _, err := parseCircomInputs(ballotProofVKey, circomProof, pubSignals)
	c.Assert(err, qt.IsNil)
	placeholder := VerifyVoteCircuit{
		CircomProof:            placeHolders.Proof,
		CircomPublicInputsHash: placeHolders.Witness,
		CircomVerificationKey:  placeHolders.Vk,
	}

	// SERVER SIDE GNARK CIRCUIT

	// print constrains
	// c.Assert(printConstrains(&placeholder), qt.IsNil)
	// init inputs
	witness := VerifyVoteCircuit{
		InputsHash: inputsHash.String(),
		// circom inputs
		MaxCount:         maxCount,
		ForceUniqueness:  forceUniqueness,
		MaxValue:         maxValue,
		MinValue:         minValue,
		MaxTotalCost:     int(math.Pow(float64(maxValue), float64(costExp))) * maxCount,
		MinTotalCost:     maxCount,
		CostExp:          costExp,
		CostFromWeight:   costFromWeight,
		Address:          address.Big(),
		UserWeight:       weight,
		EncryptionPubKey: [2]frontend.Variable{encryptionKeyX, encryptionKeyY},
		Nullifier:        nullifier,
		Commitment:       commitment,
		ProcessId:        util.BigToFF(new(big.Int).SetBytes(processID)),
		EncryptedBallot:  fBallots,
		// census proof
		CensusRoot:     testCensus.Root,
		CensusSiblings: fSiblings,
		// signature
		PublicKey: gecdsa.PublicKey[emulated.Secp256k1Fp, emulated.Secp256k1Fr]{
			X: emulated.ValueOf[emulated.Secp256k1Fp](pubKey.X),
			Y: emulated.ValueOf[emulated.Secp256k1Fp](pubKey.Y),
		},
		Signature: gecdsa.Signature[emulated.Secp256k1Fr]{
			R: emulated.ValueOf[emulated.Secp256k1Fr](rSign),
			S: emulated.ValueOf[emulated.Secp256k1Fr](sSign),
		},
		// circom proof
		CircomProof:            proof.Proof,
		CircomPublicInputsHash: proof.PublicInputs,
	}
	// generate proof
	assert := test.NewAssert(t)
	now := time.Now()
	assert.SolvingSucceeded(&placeholder, &witness,
		test.WithCurves(ecc.BLS12_377),
		test.WithBackends(backend.GROTH16))
	fmt.Println("proving tooks", time.Since(now))
}

func printConstrains(placeholder frontend.Circuit) error {
	// compile circuit
	p := profile.Start()
	now := time.Now()
	_, err := frontend.Compile(ecc.BLS12_377.ScalarField(), r1cs.NewBuilder, placeholder)
	if err != nil {
		log.Println(err)
		return err
	}
	fmt.Println("compilation tooks", time.Since(now))
	p.Stop()
	fmt.Println("constrains", p.NbConstraints())
	return nil
}

func parseCircomInputs(vKeyFile string, rawProof, rawPubSignals string) (*parser.GnarkRecursionProof, *parser.GnarkRecursionPlaceholders, *big.Int, error) {
	// load data from assets
	vKeyData, err := os.ReadFile(vKeyFile)
	if err != nil {
		return nil, nil, nil, err
	}
	// transform to gnark format
	gnarkProofData, err := parser.UnmarshalCircomProofJSON([]byte(rawProof))
	if err != nil {
		return nil, nil, nil, err
	}
	gnarkPubSignalsData, err := parser.UnmarshalCircomPublicSignalsJSON([]byte(rawPubSignals))
	if err != nil {
		return nil, nil, nil, err
	}
	gnarkVKeyData, err := parser.UnmarshalCircomVerificationKeyJSON(vKeyData)
	if err != nil {
		return nil, nil, nil, err
	}
	proof, placeHolders, err := parser.ConvertCircomToGnarkRecursion(gnarkProofData, gnarkVKeyData, gnarkPubSignalsData, true)
	if err != nil {
		return nil, nil, nil, err
	}
	// decode pub input to get the hash to sign
	inputsHash, ok := new(big.Int).SetString(gnarkPubSignalsData[0], 10)
	if !ok {
		return nil, nil, nil, fmt.Errorf("failed to decode inputs hash")
	}
	return proof, placeHolders, inputsHash, nil
}