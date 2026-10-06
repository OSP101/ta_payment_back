package service

import "testing"

func TestThaiAddress_LookupAndFormat(t *testing.T) {
	sd, ok := LookupSubDistrict(400104)
	if !ok || sd.Name != "ท่าพระ" || sd.District != "เมืองขอนแก่น" || sd.Province != "ขอนแก่น" || sd.Zip != "40260" {
		t.Fatalf("400104 = %+v, %v", sd, ok)
	}
	if got := FormatThaiAddress(" 123   ม.4 ", sd); got != "123 ม.4 ต.ท่าพระ อ.เมืองขอนแก่น จ.ขอนแก่น" {
		t.Errorf("format = %q", got)
	}
	bkk, ok := LookupSubDistrict(100101)
	if !ok {
		t.Fatal("Bangkok sub-district missing")
	}
	if got := FormatThaiAddress("1", bkk); got != "1 แขวงพระบรมมหาราชวัง เขตพระนคร กรุงเทพมหานคร" {
		t.Errorf("bangkok format = %q", got)
	}
	if _, ok := LookupSubDistrict(999999); ok {
		t.Error("unknown code accepted")
	}
}

func TestValidateProfileInput_RequiresPickedSubDistrict(t *testing.T) {
	in := TAProfile{
		StudentID: "653020111-1", Prefix: "นาย", Phone: "0812345678", NationalID: "1234567890121",
		BankName: "ธนาคารไทยพาณิชย์", AccountNo: "4091290303", AccountName: "ทดสอบ",
		SignatureSVG: "<svg><path d='M0 0 L9 9'/></svg>", AddressLine: "123", SubDistrictID: 1,
	}
	if err := validateProfileInput(&in, false); err == nil {
		t.Error("an unknown sub-district code must be refused")
	}
	in.SubDistrictID = 400104
	in.PostalCode = "40260"
	if err := validateProfileInput(&in, false); err != nil {
		t.Fatalf("valid form refused: %v", err)
	}
	if in.Address != "123 ต.ท่าพระ อ.เมืองขอนแก่น จ.ขอนแก่น" {
		t.Errorf("Address = %q", in.Address)
	}
}
