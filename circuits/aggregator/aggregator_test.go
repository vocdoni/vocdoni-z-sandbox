package aggregator

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/consensys/gnark-crypto/ecc"
	fr_bls12377 "github.com/consensys/gnark-crypto/ecc/bls12-377/fr"
	"github.com/consensys/gnark-crypto/ecc/bls12-377/fr/mimc"
	cmimc "github.com/consensys/gnark-crypto/ecc/bw6-761/fr/mimc"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/backend/groth16"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
	"github.com/consensys/gnark/std/algebra/emulated/sw_bn254"
	"github.com/consensys/gnark/std/algebra/native/sw_bls12377"
	"github.com/consensys/gnark/std/math/emulated"
	stdgroth16 "github.com/consensys/gnark/std/recursion/groth16"
	gecdsa "github.com/consensys/gnark/std/signature/ecdsa"
	"github.com/consensys/gnark/test"
	qt "github.com/frankban/quicktest"
	"github.com/iden3/go-iden3-crypto/mimc7"
	"github.com/vocdoni/arbo"
	ptest "github.com/vocdoni/gnark-crypto-primitives/testutil"
	ztest "github.com/vocdoni/vocdoni-z-sandbox/circuits/test"
	"github.com/vocdoni/vocdoni-z-sandbox/circuits/voteverifier"
	"github.com/vocdoni/vocdoni-z-sandbox/encrypt"
	"go.vocdoni.io/dvote/util"
)

const (
	nFields = 8
	nLevels = 160
)

func TestAggregatorCircuit(t *testing.T) {
	c := qt.New(t)

	// compile ballot verifier circuit
	ballotVerifierPlaceholder, err := ztest.Circom2GnarkPlaceholder()
	c.Assert(err, qt.IsNil)

	// compile vote verifier circuit
	voteVerifierPlaceholder := &voteverifier.VerifyVoteCircuit{
		CircomProof:            ballotVerifierPlaceholder.Proof,
		CircomPublicInputsHash: ballotVerifierPlaceholder.Witness,
		CircomVerificationKey:  ballotVerifierPlaceholder.Vk,
	}
	ccs, err := frontend.Compile(ecc.BLS12_377.ScalarField(), r1cs.NewBuilder, voteVerifierPlaceholder)
	c.Assert(err, qt.IsNil)
	pk, vk, err := groth16.Setup(ccs)
	c.Assert(err, qt.IsNil)

	// common process id
	processID := util.RandomBytes(20)
	// generate encryption key and user nonce k
	encryptionKey := ztest.GenerateEncryptionTestKey()
	encryptionKeyX, encryptionKeyY := encryptionKey.Point()
	// generate structs to store voters inputs
	var (
		addresses        [nVoters][]byte
		nullifiers       [nVoters]*big.Int
		commitments      [nVoters]*big.Int
		encryptedBallots [nVoters][][][]string
		proofs           [nVoters]stdgroth16.Proof[sw_bls12377.G1Affine, sw_bls12377.G2Affine]
		pubInputs        [nVoters]stdgroth16.Witness[sw_bls12377.ScalarField]
	)
	// generate users accounts and census
	privKeys, pubKeys, weights := []*ecdsa.PrivateKey{}, []ecdsa.PublicKey{}, [][]byte{}
	for i := 0; i < nVoters; i++ {
		// generate voter account
		privKey, pubKey, address, err := ztest.GenerateECDSAaccount()
		c.Assert(err, qt.IsNil)
		privKeys = append(privKeys, privKey)
		pubKeys = append(pubKeys, pubKey)
		weights = append(weights, new(big.Int).SetInt64(ztest.Weight).Bytes())
		addresses[i] = address.Bytes()
	}
	// generate a test census proof
	testCensus, err := ptest.GenerateCensusProofForTest(ptest.CensusTestConfig{
		Dir:           "../assets/census",
		ValidSiblings: 10,
		TotalSiblings: ztest.NLevels,
		KeyLen:        20,
		Hash:          arbo.HashFunctionMiMC_BLS12_377,
		BaseFiled:     arbo.BLS12377BaseField,
	}, addresses[:], weights)
	c.Assert(err, qt.IsNil)
	// generate voters inputs values and proofs
	totalPlainCipherfields := []*big.Int{}
	for i := 0; i < nVoters; i++ {
		// generate random ballot fields values
		fields := ztest.GenerateBallotFields(ztest.MaxCount, ztest.MaxValue, ztest.MinValue, ztest.ForceUniqueness > 0)
		// generate voter nonce k
		k, err := encrypt.RandK()
		c.Assert(err, qt.IsNil)
		// encrypt the ballots fields
		cipherfields, plainCipherfields := ztest.CipherBallotFields(fields, ztest.NFields, encryptionKey, k)
		encryptedBallots[i] = cipherfields
		totalPlainCipherfields = append(totalPlainCipherfields, plainCipherfields...)
		// generate user commitment and nullifier
		secret := util.RandomBytes(16)
		commitment, nullifier, err := ztest.MockedCommitmentAndNullifier(addresses[i], processID, secret)
		c.Assert(err, qt.IsNil)
		nullifiers[i] = nullifier
		commitments[i] = commitment
		// group the circom inputs to hash
		bigCircomInputs := []*big.Int{
			big.NewInt(int64(ztest.MaxCount)),
			big.NewInt(int64(ztest.ForceUniqueness)),
			big.NewInt(int64(ztest.MaxValue)),
			big.NewInt(int64(ztest.MinValue)),
			big.NewInt(int64(math.Pow(float64(ztest.MaxValue), float64(ztest.CostExp))) * int64(ztest.MaxCount)),
			big.NewInt(int64(ztest.MaxCount)),
			big.NewInt(int64(ztest.CostExp)),
			big.NewInt(int64(ztest.CostFromWeight)),
			arbo.BigToFF(arbo.BN254BaseField, new(big.Int).SetBytes(addresses[i])),
			big.NewInt(int64(ztest.Weight)),
			arbo.BigToFF(arbo.BN254BaseField, new(big.Int).SetBytes(processID)),
			encryptionKeyX,
			encryptionKeyY,
			nullifier,
			commitment,
		}
		bigCircomInputs = append(bigCircomInputs, plainCipherfields...)
		// hash the inputs
		circomInputsHash, err := mimc7.Hash(bigCircomInputs, nil)
		c.Assert(err, qt.IsNil)
		// init circom inputs
		circomInputs := map[string]any{
			"fields":           ztest.BigIntArrayToStringArray(fields, ztest.NFields),
			"max_count":        fmt.Sprint(ztest.MaxCount),
			"force_uniqueness": fmt.Sprint(ztest.ForceUniqueness),
			"max_value":        fmt.Sprint(ztest.MaxValue),
			"min_value":        fmt.Sprint(ztest.MinValue),
			"max_total_cost":   fmt.Sprint(int(math.Pow(float64(ztest.MaxValue), float64(ztest.CostExp))) * ztest.MaxCount),
			"min_total_cost":   fmt.Sprint(ztest.MaxCount),
			"cost_exp":         fmt.Sprint(ztest.CostExp),
			"cost_from_weight": fmt.Sprint(ztest.CostFromWeight),
			"address":          arbo.BigToFF(arbo.BN254BaseField, new(big.Int).SetBytes(addresses[i])).String(),
			"weight":           fmt.Sprint(ztest.Weight),
			"process_id":       arbo.BigToFF(arbo.BN254BaseField, new(big.Int).SetBytes(processID)).String(),
			"pk":               []string{encryptionKeyX.String(), encryptionKeyY.String()},
			"k":                k.String(),
			"cipherfields":     cipherfields,
			"nullifier":        nullifier.String(),
			"commitment":       commitment.String(),
			"secret":           arbo.BigToFF(arbo.BN254BaseField, new(big.Int).SetBytes(secret)).String(),
			"inputs_hash":      circomInputsHash.String(),
		}
		bCircomInputs, err := json.Marshal(circomInputs)
		c.Assert(err, qt.IsNil)
		// create the proof
		circomProof, err := ztest.Circom2GnarkProof(bCircomInputs)
		c.Assert(err, qt.IsNil)
		// transform cipherfields to gnark frontend.Variable
		emulatedBallots := [ztest.NFields][2][2]emulated.Element[sw_bn254.ScalarField]{}
		for i, c := range cipherfields {
			emulatedBallots[i] = [2][2]emulated.Element[sw_bn254.ScalarField]{
				{
					emulated.ValueOf[sw_bn254.ScalarField](c[0][0]),
					emulated.ValueOf[sw_bn254.ScalarField](c[0][1]),
				},
				{
					emulated.ValueOf[sw_bn254.ScalarField](c[1][0]),
					emulated.ValueOf[sw_bn254.ScalarField](c[1][1]),
				},
			}
		}
		// transform the inputs hash to the field of the curve used by the circuit,
		// if it is not done, the circuit will transform it during witness
		// calculation and the hash will be different
		blsCircomInputsHash := arbo.BigToFF(ecc.BLS12_377.ScalarField(), circomInputsHash)
		// sign the inputs hash with the private key
		rSign, sSign, err := ztest.SignECDSA(privKeys[i], blsCircomInputsHash.Bytes())
		// transform siblings to gnark frontend.Variable
		fSiblings := [ztest.NLevels]frontend.Variable{}
		for i, s := range testCensus.Proofs[0].Siblings {
			fSiblings[i] = frontend.Variable(s)
		}
		// hash the inputs of gnark circuit (circom inputs hash + census root)
		hFn := mimc.NewMiMC()
		hFn.Write(blsCircomInputsHash.Bytes())
		hFn.Write(testCensus.Root.Bytes())
		inputsHash := new(big.Int).SetBytes(hFn.Sum(nil))
		// init inputs
		witness := &voteverifier.VerifyVoteCircuit{
			InputsHash: inputsHash,
			// circom inputs
			MaxCount:        emulated.ValueOf[sw_bn254.ScalarField](ztest.MaxCount),
			ForceUniqueness: emulated.ValueOf[sw_bn254.ScalarField](ztest.ForceUniqueness),
			MaxValue:        emulated.ValueOf[sw_bn254.ScalarField](ztest.MaxValue),
			MinValue:        emulated.ValueOf[sw_bn254.ScalarField](ztest.MinValue),
			MaxTotalCost:    emulated.ValueOf[sw_bn254.ScalarField](int(math.Pow(float64(ztest.MaxValue), float64(ztest.CostExp))) * ztest.MaxCount),
			MinTotalCost:    emulated.ValueOf[sw_bn254.ScalarField](ztest.MaxCount),
			CostExp:         emulated.ValueOf[sw_bn254.ScalarField](ztest.CostExp),
			CostFromWeight:  emulated.ValueOf[sw_bn254.ScalarField](ztest.CostFromWeight),
			Address:         emulated.ValueOf[sw_bn254.ScalarField](new(big.Int).SetBytes(addresses[i])),
			UserWeight:      emulated.ValueOf[sw_bn254.ScalarField](ztest.Weight),
			EncryptionPubKey: [2]emulated.Element[sw_bn254.ScalarField]{
				emulated.ValueOf[sw_bn254.ScalarField](encryptionKeyX),
				emulated.ValueOf[sw_bn254.ScalarField](encryptionKeyY),
			},
			Nullifier:       emulated.ValueOf[sw_bn254.ScalarField](nullifier),
			Commitment:      emulated.ValueOf[sw_bn254.ScalarField](commitment),
			ProcessId:       emulated.ValueOf[sw_bn254.ScalarField](arbo.BigToFF(arbo.BN254BaseField, new(big.Int).SetBytes(processID))),
			EncryptedBallot: emulatedBallots,
			// census proof
			CensusRoot:     testCensus.Root,
			CensusSiblings: fSiblings,
			// signature
			Msg: emulated.ValueOf[emulated.Secp256k1Fr](blsCircomInputsHash),
			PublicKey: gecdsa.PublicKey[emulated.Secp256k1Fp, emulated.Secp256k1Fr]{
				X: emulated.ValueOf[emulated.Secp256k1Fp](pubKeys[i].X),
				Y: emulated.ValueOf[emulated.Secp256k1Fp](pubKeys[i].Y),
			},
			Signature: gecdsa.Signature[emulated.Secp256k1Fr]{
				R: emulated.ValueOf[emulated.Secp256k1Fr](rSign),
				S: emulated.ValueOf[emulated.Secp256k1Fr](sSign),
			},
			// circom proof
			CircomProof:            circomProof.Proof,
			CircomPublicInputsHash: circomProof.PublicInputs,
		}
		fullWitness, err := frontend.NewWitness(witness, ecc.BLS12_377.ScalarField())
		c.Assert(err, qt.IsNil)
		proof, err := groth16.Prove(ccs, pk, fullWitness, stdgroth16.GetNativeProverOptions(ecc.BW6_761.ScalarField(), ecc.BLS12_377.ScalarField()))
		c.Assert(err, qt.IsNil)
		proofs[i], err = stdgroth16.ValueOfProof[sw_bls12377.G1Affine, sw_bls12377.G2Affine](proof)
		c.Assert(err, qt.IsNil)
		pubInputs[i], err = stdgroth16.ValueOfWitness[sw_bls12377.ScalarField](fullWitness)
		c.Assert(err, qt.IsNil)
	}
	// compute public inputs hash
	inputs := []emulated.Element[sw_bls12377.ScalarField]{
		emulated.ValueOf[sw_bls12377.ScalarField](ztest.MaxCount),
		emulated.ValueOf[sw_bls12377.ScalarField](ztest.ForceUniqueness),
		emulated.ValueOf[sw_bls12377.ScalarField](ztest.MaxValue),
		emulated.ValueOf[sw_bls12377.ScalarField](ztest.MinValue),
		emulated.ValueOf[sw_bls12377.ScalarField](int(math.Pow(float64(ztest.MaxValue), float64(ztest.CostExp))) * ztest.MaxCount),
		emulated.ValueOf[sw_bls12377.ScalarField](ztest.MaxCount),
		emulated.ValueOf[sw_bls12377.ScalarField](ztest.CostExp),
		emulated.ValueOf[sw_bls12377.ScalarField](ztest.CostFromWeight),
		emulated.ValueOf[sw_bls12377.ScalarField](encryptionKeyX),
		emulated.ValueOf[sw_bls12377.ScalarField](encryptionKeyY),
		emulated.ValueOf[sw_bls12377.ScalarField](new(big.Int).SetBytes(processID)),
		emulated.ValueOf[sw_bls12377.ScalarField](testCensus.Root),
	}
	for _, nullifier := range nullifiers {
		inputs = append(inputs, emulated.ValueOf[sw_bls12377.ScalarField](nullifier))
	}
	for _, commitment := range commitments {
		inputs = append(inputs, emulated.ValueOf[sw_bls12377.ScalarField](commitment))
	}
	for _, address := range addresses {
		inputs = append(inputs, emulated.ValueOf[sw_bls12377.ScalarField](new(big.Int).SetBytes(address)))
	}
	for _, coord := range totalPlainCipherfields {
		inputs = append(inputs, emulated.ValueOf[sw_bls12377.ScalarField](coord))
	}
	publicHash, err := ComputePublicInputsHashFromBLS12377ToBW6761(inputs)
	c.Assert(err, qt.IsNil)

	finalVk, err := stdgroth16.ValueOfVerifyingKey[sw_bls12377.G1Affine, sw_bls12377.G2Affine, sw_bls12377.GT](vk)
	c.Assert(err, qt.IsNil)
	finalPlaceholder := &AggregatorCircuit{
		VerifyVerificationKey: finalVk,
	}
	finalWitness := &AggregatorCircuit{
		InputsHash:         publicHash,
		MaxCount:           ztest.MaxCount,
		ForceUniqueness:    ztest.ForceUniqueness,
		MaxValue:           ztest.MaxValue,
		MinValue:           ztest.MinValue,
		MaxTotalCost:       int(math.Pow(float64(ztest.MaxValue), float64(ztest.CostExp))) * ztest.MaxCount,
		MinTotalCost:       ztest.MaxCount,
		CostExp:            ztest.CostExp,
		CostFromWeight:     ztest.CostFromWeight,
		EncryptionPubKey:   [2]frontend.Variable{encryptionKeyX, encryptionKeyY},
		ProcessId:          new(big.Int).SetBytes(processID),
		CensusRoot:         testCensus.Root,
		VerifyProofs:       proofs,
		VerifyPublicInputs: pubInputs,
	}
	for i := 0; i < nVoters; i++ {
		// placeholder stuff
		finalPlaceholder.VerifyPublicInputs[i] = stdgroth16.PlaceholderWitness[sw_bls12377.ScalarField](ccs)
		finalPlaceholder.VerifyProofs[i] = stdgroth16.PlaceholderProof[sw_bls12377.G1Affine, sw_bls12377.G2Affine](ccs)
		// witness stuff
		finalWitness.Nullifiers[i] = nullifiers[i]
		finalWitness.Commitments[i] = commitments[i]
		finalWitness.Addresses[i] = new(big.Int).SetBytes(addresses[i])
		for j := 0; j < nFields; j++ {
			for n := 0; n < 2; n++ {
				for m := 0; m < 2; m++ {
					finalWitness.EncryptedBallots[i][j][n][m] = encryptedBallots[i][j][n][m]
				}
			}
		}
	}
	// generate proof
	assert := test.NewAssert(t)
	now := time.Now()
	assert.SolvingSucceeded(finalPlaceholder, finalWitness,
		test.WithCurves(ecc.BW6_761),
		test.WithBackends(backend.GROTH16))
	fmt.Println("proving tooks", time.Since(now))
}

func ComputePublicInputsHashFromBLS12377ToBW6761(publicInputs []emulated.Element[sw_bls12377.ScalarField]) (*big.Int, error) {
	h := cmimc.NewMiMC()
	var buf [fr_bls12377.Bytes]byte
	for _, input := range publicInputs {
		// Hash each limb of the emulated element
		for _, limb := range input.Limbs {
			limbValue, err := getBigIntFromVariable(limb)
			if err != nil {
				return nil, err
			}
			limbValue.FillBytes(buf[:])
			h.Write(buf[:])
		}
	}
	digest := h.Sum(nil)
	publicHash := new(big.Int).SetBytes(digest)

	return publicHash, nil
}

func getBigIntFromVariable(v frontend.Variable) (*big.Int, error) {
	switch val := v.(type) {
	case *big.Int:
		return val, nil
	case big.Int:
		return &val, nil
	case uint64:
		return new(big.Int).SetUint64(val), nil
	case int:
		return big.NewInt(int64(val)), nil
	case string:
		bi := new(big.Int)
		_, ok := bi.SetString(val, 10)
		if !ok {
			return nil, fmt.Errorf("invalid string for big.Int: %s", val)
		}
		return bi, nil
	default:
		return nil, fmt.Errorf("unsupported variable type %T", val)
	}
}