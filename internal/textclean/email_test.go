package textclean

import "testing"

// 모든 픽스처는 가짜 데이터다(실제 메일 아님).
//
// 본문(새로 쓴 부분)은 안전장치 하한(minKeepChars=100자, 원문 대비 15%)을
// 넉넉히 넘기도록 일부러 여러 문장으로 길게 작성했다 — 안전장치가 정상
// 동작을 오탐으로 되돌리지 않는지까지 함께 검증하기 위함이다.

func TestCleanEmailBody_QuoteMarkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "english_on_wrote",
			input: "안녕하세요, 확인 감사합니다. 말씀해주신 내용 반영해서 다시 정리한 문서 오늘 중으로 보내드리겠습니다.\n" +
				"혹시 추가로 검토가 필요한 부분이 있으면 언제든지 알려주세요. 금요일까지는 최종본을 마무리하겠습니다.\n\n" +
				"On Mon, Jan 5, 2026 at 3:04 PM John Doe <john@example.com> wrote:\n" +
				"> 이전 메일 내용입니다.\n" +
				"> 두 번째 줄입니다.\n",
			want: "안녕하세요, 확인 감사합니다. 말씀해주신 내용 반영해서 다시 정리한 문서 오늘 중으로 보내드리겠습니다.\n혹시 추가로 검토가 필요한 부분이 있으면 언제든지 알려주세요. 금요일까지는 최종본을 마무리하겠습니다.",
		},
		{
			name: "outlook_original_message",
			input: "네, 말씀하신 방향대로 진행하겠습니다. 일정은 다음 주 화요일 오후로 조정하는 게 좋을 것 같습니다. 참석자 명단도 함께 정리하겠습니다.\n" +
				"추가로 필요한 자료가 있으면 언제든 말씀해주세요. 회신 기다리겠습니다. 감사합니다.\n\n" +
				"-----Original Message-----\n" +
				"From: hong@example.com\n" +
				"Sent: Monday, January 5, 2026 2:00 PM\n" +
				"To: baek@example.com\n" +
				"Subject: 회의 일정 문의\n\n" +
				"이전 메일 본문입니다.\n",
			want: "네, 말씀하신 방향대로 진행하겠습니다. 일정은 다음 주 화요일 오후로 조정하는 게 좋을 것 같습니다. 참석자 명단도 함께 정리하겠습니다.\n추가로 필요한 자료가 있으면 언제든 말씀해주세요. 회신 기다리겠습니다. 감사합니다.",
		},
		{
			name: "outlook_header_block_without_dashes",
			input: "검토 결과 특별한 문제는 없어 보입니다. 다음 단계로 바로 진행해도 좋을 것 같습니다. 궁금한 점 있으면 언제든 말씀해주세요.\n" +
				"필요하면 화상 회의를 잡아서 다시 한 번 논의해보시죠. 편하신 시간 알려주세요. 감사합니다.\n\n" +
				"From: hong@example.com\n" +
				"Sent: Monday, January 5, 2026 2:00 PM\n" +
				"To: baek@example.com\n" +
				"Subject: 검토 요청드립니다\n\n" +
				"검토 부탁드립니다. 첨부파일 확인해주세요.\n",
			want: "검토 결과 특별한 문제는 없어 보입니다. 다음 단계로 바로 진행해도 좋을 것 같습니다. 궁금한 점 있으면 언제든 말씀해주세요.\n필요하면 화상 회의를 잡아서 다시 한 번 논의해보시죠. 편하신 시간 알려주세요. 감사합니다.",
		},
		{
			name: "english_forwarded_marker",
			input: "이 메일 참고하시라고 전달드립니다. 검토하시고 의견 있으면 편하게 말씀해주세요. 회신은 이번 주 안으로 부탁드립니다.\n" +
				"특히 세 번째 항목은 저희 쪽에서도 다시 확인이 필요할 것 같습니다. 감사합니다.\n\n" +
				"---------- Forwarded message ---------\n" +
				"From: someone@example.com\n" +
				"Date: Mon, Jan 5, 2026\n" +
				"Subject: 원본 제목\n\n" +
				"원본 메일 본문입니다.\n",
			want: "이 메일 참고하시라고 전달드립니다. 검토하시고 의견 있으면 편하게 말씀해주세요. 회신은 이번 주 안으로 부탁드립니다.\n특히 세 번째 항목은 저희 쪽에서도 다시 확인이 필요할 것 같습니다. 감사합니다.",
		},
		{
			name: "korean_nimi_jakseong_with_date_time",
			input: "확인했습니다. 말씀하신 대로 수정 반영해서 다시 정리했습니다. 첨부파일로 공유드립니다. 검토 부탁드립니다.\n" +
				"오늘 중으로 최종 검토까지 마치고 다시 연락드리겠습니다. 감사합니다. 좋은 하루 보내세요.\n\n" +
				"2026년 3월 5일 (목) 오후 3:04에 홍길동님이 작성:\n" +
				"> 이전 메일 인용 내용\n",
			want: "확인했습니다. 말씀하신 대로 수정 반영해서 다시 정리했습니다. 첨부파일로 공유드립니다. 검토 부탁드립니다.\n오늘 중으로 최종 검토까지 마치고 다시 연락드리겠습니다. 감사합니다. 좋은 하루 보내세요.",
		},
		{
			name: "korean_nimi_jakseong_with_email_addr",
			input: "네 알겠습니다, 요청하신 자료 준비해서 오늘 안으로 보내드리겠습니다. 검토 후 다시 회신드리겠습니다.\n" +
				"혹시 그 외에 더 필요한 항목이 있으면 편하게 알려주세요. 감사합니다. 좋은 하루 보내세요.\n\n" +
				"2026. 3. 5., 홍길동 <hong@example.com>님이 작성:\n" +
				"> 이전 메일 인용 내용\n",
			want: "네 알겠습니다, 요청하신 자료 준비해서 오늘 안으로 보내드리겠습니다. 검토 후 다시 회신드리겠습니다.\n혹시 그 외에 더 필요한 항목이 있으면 편하게 알려주세요. 감사합니다. 좋은 하루 보내세요.",
		},
		{
			name: "korean_bonaen_saram_header_block",
			input: "말씀 주신 견적서 잘 확인했습니다. 별도로 문의드릴 사항은 없습니다. 바로 계약 진행하겠습니다.\n" +
				"필요한 서류는 이번 주 안으로 보내드리겠습니다. 확인 부탁드립니다. 감사합니다. 좋은 하루 보내세요.\n\n" +
				"보낸사람 : 홍길동 <hong@example.com>\n" +
				"받는사람 : 백상엽 <baek@example.com>\n" +
				"날짜 : 2026년 3월 5일\n" +
				"제목 : 견적서 발송드립니다\n\n" +
				"견적서 첨부드립니다.\n",
			want: "말씀 주신 견적서 잘 확인했습니다. 별도로 문의드릴 사항은 없습니다. 바로 계약 진행하겠습니다.\n필요한 서류는 이번 주 안으로 보내드리겠습니다. 확인 부탁드립니다. 감사합니다. 좋은 하루 보내세요.",
		},
		{
			name: "korean_forwarded_marker",
			input: "전달받은 내용 공유드립니다. 확인 부탁드리고 의견 있으면 회신 주세요. 검토 후 알려주시면 감사하겠습니다.\n" +
				"별도로 추가 회신은 필요 없을 것 같습니다만 참고 부탁드립니다. 감사합니다.\n\n" +
				"---------- 전달된 메시지 ----------\n" +
				"보낸사람 : 관리자 <admin@example.com>\n" +
				"제목 : 공지사항\n\n" +
				"공지 원문 내용입니다.\n",
			want: "전달받은 내용 공유드립니다. 확인 부탁드리고 의견 있으면 회신 주세요. 검토 후 알려주시면 감사하겠습니다.\n별도로 추가 회신은 필요 없을 것 같습니다만 참고 부탁드립니다. 감사합니다.",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CleanEmailBody(tc.input)
			if got != tc.want {
				t.Errorf("CleanEmailBody() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestCleanEmailBody_Signatures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "rfc3676_signature_delimiter",
			input: "이번 주 안으로 회신드리겠습니다. 조금만 더 기다려주시면 감사하겠습니다. 진행 상황은 중간에 한 번 더 공유드리겠습니다.\n" +
				"확인되는 대로 바로 연락드리겠습니다. 다시 한 번 감사드리며 좋은 하루 보내세요.\n" +
				"-- \n" +
				"홍길동\n" +
				"영업팀 / 애자일소다\n" +
				"010-0000-0000\n",
			want: "이번 주 안으로 회신드리겠습니다. 조금만 더 기다려주시면 감사하겠습니다. 진행 상황은 중간에 한 번 더 공유드리겠습니다.\n확인되는 대로 바로 연락드리겠습니다. 다시 한 번 감사드리며 좋은 하루 보내세요.",
		},
		{
			name: "sent_from_my_iphone",
			input: "네 확인했습니다. 바로 처리하도록 하겠습니다. 추가로 필요한 부분 있으면 다시 연락 주세요. 오늘 중으로 마무리해서 회신드리겠습니다.\n" +
				"확인되는 대로 결과 바로 공유드리겠습니다. 감사합니다. 좋은 하루 되세요.\n\n" +
				"Sent from my iPhone",
			want: "네 확인했습니다. 바로 처리하도록 하겠습니다. 추가로 필요한 부분 있으면 다시 연락 주세요. 오늘 중으로 마무리해서 회신드리겠습니다.\n확인되는 대로 결과 바로 공유드리겠습니다. 감사합니다. 좋은 하루 되세요.",
		},
		{
			name: "korean_iphone_signature",
			input: "일정 조율해주셔서 감사합니다. 다음 회의 때 다시 뵙겠습니다. 편하신 시간대로 맞춰볼게요. 자세한 안건은 회의 전에 미리 공유드리겠습니다.\n" +
				"추가로 필요한 자료도 함께 정리해서 보내드리겠습니다. 감사합니다.\n\n" +
				"iPhone에서 보냄",
			want: "일정 조율해주셔서 감사합니다. 다음 회의 때 다시 뵙겠습니다. 편하신 시간대로 맞춰볼게요. 자세한 안건은 회의 전에 미리 공유드리겠습니다.\n추가로 필요한 자료도 함께 정리해서 보내드리겠습니다. 감사합니다.",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CleanEmailBody(tc.input)
			if got != tc.want {
				t.Errorf("CleanEmailBody() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestCleanEmailBody_QuotedLinesWithoutMarker(t *testing.T) {
	t.Parallel()

	// 명시적 인용 마커 없이 '>' 인용 줄만 섞여 있는 경우에도 인용 줄은
	// 제거되어야 한다(방어적 처리).
	input := "네, 그 방향으로 진행하겠습니다. 일정표도 같이 정리해서 공유드리겠습니다. 필요하신 자료도 함께 준비하겠습니다.\n" +
		"> 이건 인용된 예전 문장입니다.\n" +
		"확인 후 다시 답변드리겠습니다. 감사합니다. 좋은 하루 보내시고 다음 주에 뵙겠습니다.\n"
	want := "네, 그 방향으로 진행하겠습니다. 일정표도 같이 정리해서 공유드리겠습니다. 필요하신 자료도 함께 준비하겠습니다.\n확인 후 다시 답변드리겠습니다. 감사합니다. 좋은 하루 보내시고 다음 주에 뵙겠습니다."

	got := CleanEmailBody(input)
	if got != want {
		t.Errorf("CleanEmailBody() =\n%q\nwant\n%q", got, want)
	}
}

func TestCleanEmailBody_SafetyNet(t *testing.T) {
	t.Parallel()

	t.Run("short_mail_returns_original_when_result_too_small", func(t *testing.T) {
		t.Parallel()
		// 정리 후 결과가 100자 미만이 되므로 안전장치가 원문을 그대로 반환해야 한다.
		input := "감사합니다.\n" +
			"-- \n" +
			"홍길동 드림\n"
		got := CleanEmailBody(input)
		if got != input {
			t.Errorf("CleanEmailBody() = %q, want original %q (safety net should trigger)", got, input)
		}
	})

	t.Run("newsletter_without_quote_markers_is_unchanged", func(t *testing.T) {
		t.Parallel()
		// 뉴스레터류는 인용 마커가 없으므로 그대로 보존되어야 한다("제목:"이
		// 한 번만 등장하는 것으로는 헤더 블록으로 오판하지 않는다).
		input := "이번 주 뉴스레터를 보내드립니다.\n\n" +
			"제목: 2월 셋째 주 소식\n\n" +
			"이번 주에는 신규 기능 세 가지를 소개합니다. 첫 번째는 검색 개선이고,\n" +
			"두 번째는 알림 기능 강화이며, 세 번째는 모바일 UI 개편입니다.\n" +
			"자세한 내용은 아래 링크에서 확인하실 수 있습니다.\n" +
			"구독을 해지하시려면 이 메일 하단의 링크를 클릭해주세요."
		got := CleanEmailBody(input)
		if got != input {
			t.Errorf("CleanEmailBody() =\n%q\nwant unchanged original\n%q", got, input)
		}
	})

	t.Run("plain_mail_without_quotes_is_unchanged", func(t *testing.T) {
		t.Parallel()
		input := "안녕하세요, 다름이 아니라 다음 주 미팅 일정 관련해서 연락드립니다.\n" +
			"화요일 오후 2시가 가능하실지 확인 부탁드립니다.\n" +
			"편하신 시간 알려주시면 바로 일정 잡겠습니다."
		got := CleanEmailBody(input)
		if got != input {
			t.Errorf("CleanEmailBody() =\n%q\nwant unchanged original\n%q", got, input)
		}
	})

	t.Run("empty_input_returns_empty", func(t *testing.T) {
		t.Parallel()
		if got := CleanEmailBody(""); got != "" {
			t.Errorf("CleanEmailBody(\"\") = %q, want \"\"", got)
		}
	})
}

func TestCleanEmailBody_BlankLineCollapse(t *testing.T) {
	t.Parallel()

	// 인용/서명 마커가 없어도 3개 이상 연속된 빈 줄은 항상 2개로 정리된다
	// (정보 손실 없는 순수 공백 정규화).
	input := "첫 번째 문단입니다. 내용을 조금 더 길게 작성해 안전장치 기준을 넉넉히 넘기도록 합니다. 여러 문장을 덧붙여 충분한 길이를 확보합니다.\n\n\n\n\n" +
		"두 번째 문단입니다. 여기도 마찬가지로 충분히 긴 문장으로 채웁니다. 이렇게 하면 안전장치 걱정 없이 정리 로직만 검증할 수 있습니다."
	want := "첫 번째 문단입니다. 내용을 조금 더 길게 작성해 안전장치 기준을 넉넉히 넘기도록 합니다. 여러 문장을 덧붙여 충분한 길이를 확보합니다.\n\n두 번째 문단입니다. 여기도 마찬가지로 충분히 긴 문장으로 채웁니다. 이렇게 하면 안전장치 걱정 없이 정리 로직만 검증할 수 있습니다."

	got := CleanEmailBody(input)
	if got != want {
		t.Errorf("CleanEmailBody() =\n%q\nwant\n%q", got, want)
	}
}

func TestCleanForChunking_Dispatch(t *testing.T) {
	t.Parallel()

	quoted := "안녕하세요, 확인했습니다. 반영해서 다시 정리한 내용 오늘 중으로 보내드리겠습니다. 검토 부탁드리고 필요하면 다시 회신 주세요.\n" +
		"필요한 부분 있으면 편하게 말씀해주세요. 감사합니다. 좋은 하루 보내세요.\n\n" +
		"On Mon, Jan 5, 2026 at 3:04 PM John Doe <john@example.com> wrote:\n" +
		"> 이전 메일 내용입니다.\n"

	t.Run("gmail_applies_cleaning", func(t *testing.T) {
		t.Parallel()
		got := CleanForChunking("gmail", quoted)
		want := CleanEmailBody(quoted)
		if got != want {
			t.Errorf("CleanForChunking(gmail) =\n%q\nwant\n%q", got, want)
		}
		if got == quoted {
			t.Error("CleanForChunking(gmail) should have stripped the quoted reply, but returned input unchanged")
		}
	})

	t.Run("non_gmail_passes_through_unchanged", func(t *testing.T) {
		t.Parallel()
		for _, src := range []string{"slack", "filesystem", "sms", "call", ""} {
			if got := CleanForChunking(src, quoted); got != quoted {
				t.Errorf("CleanForChunking(%q) = %q, want unchanged input", src, got)
			}
		}
	})
}
