@echo off
cd /d "%~dp0"
lanforge.exe
if errorlevel 1 pause
