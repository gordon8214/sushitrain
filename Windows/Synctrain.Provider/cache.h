// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
// A separate NTFS copy of fetched content must not grow without bound. Pinned
// content and unsaved edits are retained regardless of the cache preference.
void trimNativeCache() {
    static ULONGLONG lastTrim{};
    auto now=GetTickCount64();if(lastTrim && now-lastTrim<(testRegistration?1000ULL:30000ULL))return;lastTrim=now;
    auto storage=request(L"files.storage").result.GetObject();
    auto available=std::max<int64_t>(0,static_cast<int64_t>(storage.GetNamedNumber(L"budget"))-static_cast<int64_t>(storage.GetNamedNumber(L"used")));
    struct Candidate {size_t index;int64_t bytes;uint64_t accessed;};
    std::vector<Candidate> candidates;std::vector<size_t> requested;int64_t total{};
    for(size_t i=0;i<records.size();++i){
        auto const& record=records[i];auto entry=record.GetNamedObject(L"Entry");
        if(entry.GetNamedNumber(L"Kind")!=0 || entry.GetNamedBoolean(L"Keep",false))continue;
        try{
            auto path=fs::path(rootPath)/std::wstring(record.GetNamedString(L"Path"));auto file=attributes(path);
            std::vector<std::byte> buffer(8192);auto info=reinterpret_cast<CF_PLACEHOLDER_STANDARD_INFO*>(buffer.data());
            if(FAILED(CfGetPlaceholderInfo(file.get(),CF_PLACEHOLDER_INFO_STANDARD,info,static_cast<DWORD>(buffer.size()),nullptr)) || info->PinState==CF_PIN_STATE_PINNED)continue;
            if(info->PinState==CF_PIN_STATE_UNPINNED && !legacyPins(record)){requested.push_back(i);continue;}
            total+=info->OnDiskDataSize.QuadPart;
            if(!info->OnDiskDataSize.QuadPart || info->InSyncState!=CF_IN_SYNC_STATE_IN_SYNC || record.HasKey(L"Capture") || record.HasKey(L"Create") || record.GetNamedBoolean(L"NeedsCapture",false) || record.GetNamedBoolean(L"AwaitingSync",false))continue;
            BY_HANDLE_FILE_INFORMATION basic{};check_bool(GetFileInformationByHandle(file.get(),&basic));
            candidates.push_back({i,info->OnDiskDataSize.QuadPart,(uint64_t(basic.ftLastAccessTime.dwHighDateTime)<<32)|basic.ftLastAccessTime.dwLowDateTime});
        }catch(hresult_error const&){} // Open editors and vanished entries remain intact.
    }
    // Free up space requests that waited for an editor to close or an upload to finish
    for(auto index:requested)freeUp(records[index],fs::path(rootPath)/std::wstring(records[index].GetNamedString(L"Path")),false);
    std::sort(candidates.begin(),candidates.end(),[](auto const& a,auto const& b){return a.accessed<b.accessed;});
    for(auto const& candidate:candidates){
        if(total<=available)break;
        try{if(dehydrate(records[candidate.index]))total-=candidate.bytes;}
        catch(hresult_error const&){} // Cache limits never discard unconfirmed local work.
    }
}
