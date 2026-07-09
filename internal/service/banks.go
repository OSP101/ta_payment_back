package service

// Thai bank list mirrored from the frontend (app/lib/banks.ts). Kept here so
// the server can reject bank names it doesn't recognise and enforce the same
// account-number length the UI hints at — a client-side check alone is not
// enough because the API also serves scripted / SSO clients.

type thaiBank struct {
	name       string
	accountLen []int
}

var thaiBanks = []thaiBank{
	{"ธนาคารกรุงเทพ", []int{10}},
	{"ธนาคารกสิกรไทย", []int{10}},
	{"ธนาคารกรุงไทย", []int{10}},
	{"ธนาคารทหารไทยธนชาต", []int{10}},
	{"ธนาคารไทยพาณิชย์", []int{10}},
	{"ธนาคารซีไอเอ็มบี ไทย", []int{10}},
	{"ธนาคารยูโอบี", []int{10}},
	{"ธนาคารกรุงศรีอยุธยา", []int{10}},
	{"ธนาคารออมสิน", []int{12}},
	{"ธนาคารอาคารสงเคราะห์", []int{10}},
	{"ธนาคารเพื่อการเกษตรและสหกรณ์การเกษตร", []int{10, 12}},
	{"ธนาคารอิสลามแห่งประเทศไทย", []int{10}},
	{"ธนาคารทิสโก้", []int{10}},
	{"ธนาคารเกียรตินาคินภัทร", []int{10}},
	{"ธนาคารไอซีบีซี (ไทย)", []int{10}},
	{"ธนาคารไทยเครดิต เพื่อรายย่อย", []int{10}},
	{"ธนาคารแลนด์ แอนด์ เฮ้าส์", []int{10}},
}

func lookupBank(name string) *thaiBank {
	for i := range thaiBanks {
		if thaiBanks[i].name == name {
			return &thaiBanks[i]
		}
	}
	return nil
}

func acceptsAccountLen(b *thaiBank, n int) bool {
	for _, want := range b.accountLen {
		if want == n {
			return true
		}
	}
	return false
}
