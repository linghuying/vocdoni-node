package zk

// func TestProtobufZKProofToCircomProof(t *testing.T) {
// 	protoProof := &models.ProofZkSNARK{
// 		A: []string{
// 			"17569240301865190069940703408776620561950070507358357198130890464508555015742",
// 			"14719281396036152924308513019111720587276284390020911371919541479835430607528",
// 			"1",
// 		},
// 		B: []string{
// 			"19366523330111704407267566338410994924319459949138724547828188860719056192113",
// 			"3554431156699466263343064300468289853516840393068286815512868805374822045471",
// 			"7069001739799325309551446576989712671469685847725867674777886436500905588451",
// 			"9519609195825772265125524447464801412742967232326200600178197674408796399758",
// 			"1",
// 			"0",
// 		},
// 		C: []string{
// 			"1803067082675811010286176187174786634523306099072027370858753233512049893073",
// 			"12821812994233574817558778896965446058705557189286765124075749479926329638171",
// 			"1",
// 		},
// 		PublicInputs: []string{"1", "2", "3"},
// 	}
// 	proof, pubInputs, err := ProtobufZKProofToCircomProof(protoProof)
// 	qt.Assert(t, err, qt.IsNil)

// 	expectedStr := `{"pi_a":"26d7d66de7e4ed7fa7abf7078ef9e4e45bf0000787c451874ee9f31c930bb63e208ad16ae0f6916670dcf183c1d23efe1e2ddc45dbb138d922c79ad1b4bcdaa8","pi_b":"07dbbc9b161442d63d59869a61a2b1724f49f8ce7cd83f92bc33edf4e697d71f2ad1105288e9d12915b079725ad629845a3c6cdd4311b21778be06cc82ae8271150be869d01f1fcb87fb7f4930e690c3b873d29529f3d9db0f8021253806a88e0fa0e9c753288b4110ef539418f37c30de082a0bc60083222e8ca6694b2ef6e3","pi_c":"03fc7ff321b29905a8a64f324906272e69c9a0e7b097b523ef57fc6a320f5ed11c58e39436354cb151affb97a664401f1eb6b54a9f3a6a366543bf6813f5fd1b"}`

// 	proofJSON, err := json.Marshal(proof)
// 	qt.Assert(t, err, qt.IsNil)
// 	qt.Assert(t, expectedStr, qt.Equals, string(proofJSON))
// 	a := []*big.Int{big.NewInt(1), big.NewInt(2), big.NewInt(3)}
// 	for i := range a {
// 		qt.Assert(t, a[i].String(), qt.Equals, pubInputs[i].String())
// 	}
// }
