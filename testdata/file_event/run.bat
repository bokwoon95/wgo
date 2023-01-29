@echo off

del /q "testdata\file_event\foo.txt" >nul 2>&1
rmdir /s /q "testdata\file_event\internal" >nul 2>&1

echo add file
echo foo | set /p=foo>"testdata\file_event\foo.txt"
timeout /t 2 /nobreak >nul

echo edit file
echo fighters>>"testdata\file_event\foo.txt"
timeout /t 2 /nobreak >nul

echo create nested directory
mkdir "testdata\file_event\internal\baz" >nul 2>&1
echo bar>"testdata\file_event\internal\bar.txt"
echo baz>"testdata\file_event\internal\baz\baz.txt"
timeout /t 2 /nobreak >nul

del /q "testdata\file_event\foo.txt"
rmdir /s /q "testdata\file_event\internal" >nul 2>&1
