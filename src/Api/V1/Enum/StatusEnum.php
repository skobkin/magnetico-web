<?php

declare(strict_types=1);

namespace App\Api\V1\Enum;

enum StatusEnum: string
{
    case Success = 'success';
    case Error = 'error';
    case Fail = 'fail';
    case Unknown = 'unknown';
}
